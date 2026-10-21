package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newIdemTestServer 只挂幂等包装，handler 按 calls 计数，status 由测试指定。
func newIdemTestServer(status *int, calls *int) (*Server, http.HandlerFunc) {
	s := &Server{idem: newMemIdemStore(time.Minute)}
	h := s.idempotent("test", func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(*status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return s, h
}

func idemRequest(h http.HandlerFunc, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/mediasvc/test", strings.NewReader(body))
	req.Header.Set(idempotencyHeader, key)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// 同一个键、同一请求体：第二次不再执行 handler，原样回放。
func TestIdempotentReplaysSuccessfulResult(t *testing.T) {
	status, calls := http.StatusOK, 0
	_, h := newIdemTestServer(&status, &calls)
	first := idemRequest(h, "k1", `{"a":1}`)
	second := idemRequest(h, "k1", `{"a":1}`)
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if second.Code != http.StatusOK || second.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("回放的结果必须带 Idempotent-Replayed 头")
	}
}

// 同一个键、不同请求体：400（invalid_request），不执行。
func TestIdempotentRejectsReusedKeyWithDifferentBody(t *testing.T) {
	status, calls := http.StatusOK, 0
	_, h := newIdemTestServer(&status, &calls)
	idemRequest(h, "k2", `{"a":1}`)
	rec := idemRequest(h, "k2", `{"a":2}`)
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（invalid_request）", rec.Code)
	}
}

// 同一个键正在处理中：409 + Retry-After，不重复执行。
func TestIdempotentBusyWhileInFlight(t *testing.T) {
	status, calls := http.StatusOK, 0
	s, h := newIdemTestServer(&status, &calls)
	if _, state, _, _ := s.idem.begin(context.Background(), "test|k3", [32]byte{}); state != idemFresh {
		t.Fatal("第一次应占用成功")
	}
	rec := idemRequest(h, "k3", `{"a":1}`)
	if calls != 0 {
		t.Fatalf("处理中的键不应再执行 handler，calls = %d", calls)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d retry-after = %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// 错误结果不缓存：第一次失败，同一个键再来一次会重新执行。
func TestIdempotentDoesNotCacheErrors(t *testing.T) {
	status, calls := http.StatusUnprocessableEntity, 0
	_, h := newIdemTestServer(&status, &calls)
	idemRequest(h, "k4", `{"a":1}`)
	status = http.StatusOK
	rec := idemRequest(h, "k4", `{"a":1}`)
	if calls != 2 {
		t.Fatalf("handler calls = %d, want 2（错误不缓存）", calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec.Code)
	}
}

// handler panic 后：调用方得到 500 internal（可重试，不是连接被断开），"处理中"标记释放，
// 同一个键的重试不能是 503 in_flight，也不缓存这次失败。
func TestIdempotentPanicReleasesInflightMark(t *testing.T) {
	s := &Server{idem: newMemIdemStore(time.Minute)}
	panicked := true
	h := s.idempotent("test", func(w http.ResponseWriter, r *http.Request) {
		if panicked {
			panic("boom")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	first := idemRequest(h, "k-panic", `{"a":1}`)
	if first.Code != http.StatusInternalServerError || !strings.Contains(first.Body.String(), `"code":"internal"`) ||
		!strings.Contains(first.Body.String(), `"retryable":true`) {
		t.Fatalf("panic 应返回 500 internal 且可重试: status=%d body=%s", first.Code, first.Body.String())
	}

	panicked = false
	rec := idemRequest(h, "k-panic", `{"a":1}`)
	if rec.Code == http.StatusServiceUnavailable || rec.Code == http.StatusConflict {
		t.Fatalf("panic 后同键重试仍被占用: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("重试应重新执行并成功: status=%d", rec.Code)
	}
}
