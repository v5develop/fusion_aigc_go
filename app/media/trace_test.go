package media

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer 让捕获的日志在 httptest 服务的 goroutine 与测试 goroutine 之间安全读写。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog 把标准库 log 输出重定向到内存，测试结束时恢复。
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return buf
}

// 带头请求：响应头原样回写同一个值。
func TestTraceHeaderEchoedWhenProvided(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	req.Header.Set(TraceHeader, "AiCreation_abc123")
	resp := send(t, req, "")
	if got := resp.Header.Get(TraceHeader); got != "AiCreation_abc123" {
		t.Fatalf("响应头 X-Trace-Id = %q, want AiCreation_abc123", got)
	}
}

// 不带头：服务生成一个非空追踪号并回写。
func TestTraceHeaderGeneratedWhenMissing(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	resp := send(t, req, "")
	got := resp.Header.Get(TraceHeader)
	if len(got) != 32 {
		t.Fatalf("生成的 X-Trace-Id = %q, want 32 位 hex", got)
	}
}

// 不合法的头（含空格等）不被原样采用，换成生成的号。
func TestTraceHeaderInvalidValueReplaced(t *testing.T) {
	if validTraceID("bad value\n") || validTraceID("") || validTraceID(strings.Repeat("a", maxTraceIDLen+1)) {
		t.Fatal("非法追踪号必须被拒绝")
	}
	if !validTraceID("AiCreation_abc-1.2:3") {
		t.Fatal("合法追踪号被拒绝")
	}
}

// 访问日志：一行含 trace_id、方法、路径（不含 query）、状态码、耗时、错误码。
func TestAccessLogCarriesTraceID(t *testing.T) {
	logs := captureLog(t)
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/mediasvc/probe?secret=zzz",
		strings.NewReader(`{"source":{"url":"http://x/a.mp4"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TraceHeader, "AiCreation_access01")
	resp := send(t, req, "") // 没有 token：401
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	out := logs.String()
	for _, want := range []string{
		"media access trace_id=AiCreation_access01",
		"method=POST",
		"path=/v1/mediasvc/probe",
		"status=401",
		"code=unauthorized",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("访问日志缺少 %q，实际：\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret=zzz") {
		t.Fatalf("访问日志不得含 query：\n%s", out)
	}
}

// 错误路径：内部错误的原始原因带追踪号写日志，响应体仍只有固定说明。
func TestInternalErrorLogCarriesTraceID(t *testing.T) {
	logs := captureLog(t)
	h := withTrace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, errors.New("boom-internal"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mediasvc/compose", nil)
	req.Header.Set(TraceHeader, "AiCreation_err42")
	h.ServeHTTP(rec, req)
	out := logs.String()
	if !strings.Contains(out, "media internal error trace_id=AiCreation_err42") || !strings.Contains(out, "boom-internal") {
		t.Fatalf("内部错误日志缺少追踪号或原因：\n%s", out)
	}
	if strings.Contains(rec.Body.String(), "boom-internal") {
		t.Fatal("原始错误不得回给调用方")
	}
	if !strings.Contains(out, "media access trace_id=AiCreation_err42 method=POST path=/v1/mediasvc/compose status=500") ||
		!strings.Contains(out, "code=internal check=internal.unexpected") {
		t.Fatalf("访问日志错误码不对：\n%s", out)
	}
}

// /readyz：对象存储可达（本地存储 HEAD 不存在的对象也算可达）→ 200；/healthz 不受影响。
func TestReadyz(t *testing.T) {
	ts := newTestServer(t, Config{})
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz status=%d", resp.StatusCode)
	}
}

type headFailStore struct {
	ObjectStore
	err error
}

func (h headFailStore) Head(context.Context, string, string) (int64, error) { return 0, h.err }

// 对象存储不可达（非「不存在」错误）→ /readyz 503；「不存在」→ 200。
func TestReadyzObjectStoreDown(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{{errors.New("dial timeout"), 503}, {errObjectNotFound, 200}} {
		s := &Server{objects: headFailStore{err: c.err}}
		rec := httptest.NewRecorder()
		s.newReady().Handler()(rec, httptest.NewRequest("GET", "/readyz", nil))
		if rec.Code != c.want {
			t.Fatalf("err=%v code=%d want %d", c.err, rec.Code, c.want)
		}
	}
}
