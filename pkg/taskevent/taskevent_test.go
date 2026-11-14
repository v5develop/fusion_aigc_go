package taskevent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"
)

type call struct {
	q    string
	args []any
}

type fakeDB struct {
	mu    sync.Mutex
	calls []call
	err   error
	gate  chan struct{} // 非 nil：ExecCtx 先等它（模拟慢库）
}

func (f *fakeDB) ExecCtx(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{q, args})
	return nil, f.err
}

func (f *fakeDB) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// rows 把所有多行 INSERT 的参数切回一行一行。
func (f *fakeDB) rows() [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]any
	for _, c := range f.calls {
		for i := 0; i+RowArgs <= len(c.args); i += RowArgs {
			out = append(out, c.args[i:i+RowArgs])
		}
	}
	return out
}

func flush(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// arg 下标见 insertSQL 的列顺序。
const (
	aTask = iota
	aTS
	aStep
	aKind
	aStatus
	aClass
	aCode
	aCheck
	aMsg
	aAttempt
	aRunNo
	aExtType
	aExtRef
	aTrace
	aSource
	aActor
	aDedupe
	aPayload
)

func str(v any) string {
	if ns, ok := v.(sql.NullString); ok {
		return ns.String
	}
	return v.(string)
}

func TestSanitizeCases(t *testing.T) {
	cases := []struct {
		name, in string
		bad      []string
		want     []string
	}{
		{"url 整段换 [url]", "拉取失败 https://oss.example.com/a/b.png?Expires=1&Signature=abc#frag 以及 https://u:p@h.com/x?y=1",
			[]string{"oss.example.com", "Expires", "Signature=abc", "u:p@", "h.com", "frag"}, []string{"[url]"}},
		{"任意 scheme", "s3://bucket/key?x=1 oss://b/k ftp://h/f mr://abc aig://gw/key",
			[]string{"bucket", "oss://", "ftp://", "mr://", "aig://", "gw/key"}, []string{"[url]"}},
		{"jwt", "token 失效 eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c 结束",
			[]string{"eyJhbGci", "SflKxwRJ"}, nil},
		{"bearer", "Authorization: Bearer abcdefghij1234567", []string{"abcdefghij1234567"}, nil},
		{"key=value", "password=hunter2 api_key=AKIA123456 secret: s3cr3t access-token=zzz999", []string{"hunter2", "AKIA123456", "s3cr3t", "zzz999"}, nil},
		{"json 形式", `{"token":"abc123XYZ","password": "p@ss"}`, []string{"abc123XYZ", "p@ss"}, nil},
		{"sk-", "key sk-abcdefgh12345678", []string{"sk-abcdefgh"}, nil},
	}
	for _, c := range cases {
		out := Sanitize(c.in)
		for _, bad := range c.bad {
			if strings.Contains(out, bad) {
				t.Errorf("%s: 脱敏后仍含 %q: %s", c.name, bad, out)
			}
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: 应含 %q: %s", c.name, w, out)
			}
		}
	}
	if got := Sanitize("普通文案 无凭证"); got != "普通文案 无凭证" {
		t.Fatalf("普通文本不应被改: %q", got)
	}
}

func TestFromErrorNeverCarriesBody(t *testing.T) {
	ge := &aigateway.Error{HTTPStatus: 422, Code: "content_policy", Message: "prompt 'SECRET 提示词' https://x/y?sig=1"}
	f := FromError(fmt.Errorf("wrap: %w", ge), "chk")
	if f.Class != "GATEWAY" || f.Code != "content_policy" || f.Check != "chk" || strings.Contains(f.Message, "SECRET") || strings.Contains(f.Message, "sig=") {
		t.Fatalf("%+v", f)
	}
	je := xerr.New(40001, "含提示词的 Java 业务消息")
	f = FromError(je, "chk")
	if f.Class != "BUSINESS" || f.Code != "40001" || strings.Contains(f.Message, "提示词") {
		t.Fatalf("%+v", f)
	}
	f = FromError(errors.New("dial tcp: "+strings.Repeat("长", 300)+" https://a/b?c=d"), "chk")
	if f.Class != "LOCAL" || utf8.RuneCountInString(f.Message) > 120 || strings.Contains(f.Message, "a/b") {
		t.Fatalf("%+v", f)
	}
}

func TestSanitizeTruncatesByRune(t *testing.T) {
	out := Sanitize(strings.Repeat("字", 800))
	if n := utf8.RuneCountInString(out); n != 500 || !utf8.ValidString(out) {
		t.Fatalf("应按 rune 截到 500，实际 %d valid=%v", n, utf8.ValidString(out))
	}
}

func TestCleanAttrs(t *testing.T) {
	got := CleanAttrs(Attrs{
		"model_group": "img.x", "points": 12, "ok": true, "elapsed_ms": 3 * time.Second,
		"prompt": "秘密", "signed_url": "https://a?b=c", "nested": map[string]any{"a": 1}, "Bad-Key": 1,
		"reason": "hard_cap https://a.com/x?sig=1", "empty": "",
	})
	if len(got) != 5 || got["reason"] != "hard_cap [url]" || got["model_group"] != "img.x" || got["points"] != 12 || got["ok"] != true || got["elapsed_ms"] != int64(3000) {
		t.Fatalf("attrs = %v", got)
	}
}

func TestDedupeKey(t *testing.T) {
	if k := DedupeKey("span_end", "go.canvas/wait", 2); k != "span_end:go.canvas/wait:2" {
		t.Fatal(k)
	}
}

func TestWriteInsertIgnoreAndColumns(t *testing.T) {
	db := &fakeDB{}
	w := New(db)
	w.Write(context.Background(), Event{
		TaskID: "AiCreation_LF_x", StepPath: "go.canvas/wait", Kind: KindSpanEnd, Status: StatusTimeout,
		FailureClass: "TIMEOUT", FailureCode: "TIMEOUT", CheckCode: "canvas.timeout", Message: "超时 https://a.com/x?sig=1",
		Attempt: 2, ExternalType: ExternalGatewayCall, ExternalRef: "call_abc", Attrs: Attrs{"reason": "hard_cap"},
		TS: time.Date(2026, 11, 13, 0, 0, 0, 5e6, time.UTC),
	})
	flush(t, w)
	if db.n() != 1 {
		t.Fatalf("calls=%d", db.n())
	}
	c := db.calls[0]
	if !strings.HasPrefix(c.q, "INSERT IGNORE INTO afv_task_event") {
		t.Fatal(c.q)
	}
	if strings.Count(c.q, "?") != len(c.args) {
		t.Fatalf("占位符 %d 与参数 %d 不一致", strings.Count(c.q, "?"), len(c.args))
	}
	if str(c.args[aTS]) != "2026-11-13 08:00:00.005" {
		t.Fatalf("ts=%v", c.args[aTS])
	}
	if str(c.args[aTask]) != "AiCreation_LF_x" || str(c.args[aTrace]) != "AiCreation_LF_x" || str(c.args[aSource]) != "go" {
		t.Fatalf("task/trace/source: %v", c.args)
	}
	if str(c.args[aDedupe]) != "span_end:go.canvas/wait:2" || str(c.args[aStatus]) != "timeout" {
		t.Fatalf("dedupe/status: %v", c.args)
	}
	if strings.Contains(str(c.args[aMsg]), "sig=") || !strings.Contains(str(c.args[aMsg]), "[url]") {
		t.Fatalf("message 未脱敏: %v", c.args[aMsg])
	}
	if p := str(c.args[aPayload]); p != `{"attrs":{"reason":"hard_cap"}}` {
		t.Fatalf("payload=%s", p)
	}
}

func TestWriteSkipsWithoutTaskAndNilSafe(t *testing.T) {
	db := &fakeDB{}
	w0 := New(db)
	w0.Write(context.Background(), Event{Kind: KindMark})
	flush(t, w0)
	if db.n() != 0 {
		t.Fatal("无 task_id 不应写")
	}
	var w *Writer
	w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark})
	w.Start(context.Background(), "x", "s", 0, nil).OK(context.Background(), nil)
	w.LinkGatewayCall(context.Background(), "x", "s", "call_1", 0, nil)
	if err := w.Close(context.Background()); err != nil || w.Flush(context.Background()) != nil || w.Dropped() != 0 {
		t.Fatal("nil writer 方法应是空操作")
	}
	if New(nil) != nil {
		t.Fatal("nil db 应得到 nil writer")
	}
}

// 库很慢（卡住）时 Write 立即返回，不等库。
func TestWriteNeverBlocksWhenDBSlow(t *testing.T) {
	db := &fakeDB{gate: make(chan struct{})}
	w := New(db).WithTimeout(time.Hour)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "a", Seq: i})
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("库卡住时 1000 次 Write 应立即返回，耗时 %s", d)
	}
	close(db.gate)
	flush(t, w)
	if got := len(db.rows()); got != 1000 {
		t.Fatalf("放开后应全部落库，rows=%d", got)
	}
}

// 库持续报错：不阻塞调用方；重试几次后整批丢弃并计数。
func TestWriteDBDownDropsAfterRetries(t *testing.T) {
	db := &fakeDB{err: errors.New("boom")}
	w := New(db)
	w.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	start := time.Now()
	w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "a"})
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Write 不应等库")
	}
	flush(t, w)
	if db.n() != 1+batchRetries {
		t.Fatalf("应首发 + %d 次重试，实际 %d", batchRetries, db.n())
	}
	if w.Dropped() != 1 || w.Written() != 0 {
		t.Fatalf("dropped=%d written=%d", w.Dropped(), w.Written())
	}
	// 之后库恢复，后续事件照常写
	db.mu.Lock()
	db.err = nil
	db.mu.Unlock()
	w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "b"})
	flush(t, w)
	if w.Written() != 1 {
		t.Fatalf("written=%d", w.Written())
	}
}

// 队列满：丢弃新事件并计数，不阻塞。
func TestWriteDropsWhenQueueFull(t *testing.T) {
	db := &fakeDB{gate: make(chan struct{})}
	w := New(db).WithTimeout(time.Hour)
	w.queue = make(chan []any, 10)
	// 批大小固定为 1：worker 取走 1 行就卡在 Exec 上；否则它会边收边攒批，负载高时一次收走远超 1 行，丢弃数不确定
	w.batchSize = 1
	for i := 0; i < 100; i++ {
		w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "a", Seq: i})
	}
	// worker 取走 1 行后卡在 Exec；队列再容 10 行，其余被丢
	if w.Dropped() < 80 {
		t.Fatalf("队列容量 10，应丢掉大部分，dropped=%d", w.Dropped())
	}
	close(db.gate)
	flush(t, w)
	if int64(len(db.rows()))+w.Dropped() != 100 {
		t.Fatalf("落库 %d + 丢弃 %d 应等于 100", len(db.rows()), w.Dropped())
	}
}

// 批量：积压的事件合并成多行 INSERT，每批不超过 batchSize。
func TestWriteBatchesRows(t *testing.T) {
	db := &fakeDB{gate: make(chan struct{})}
	w := New(db).WithTimeout(time.Hour)
	w.batchSize = 50
	for i := 0; i < 500; i++ {
		w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "a", Seq: i})
	}
	close(db.gate)
	flush(t, w)
	if len(db.rows()) != 500 {
		t.Fatalf("rows=%d", len(db.rows()))
	}
	if db.n() >= 500 || db.n() < 10 {
		t.Fatalf("应按 ≤50 行一批合并，批数=%d", db.n())
	}
	for _, c := range db.calls {
		if len(c.args)/RowArgs > 50 || strings.Count(c.q, "?") != len(c.args) || !strings.HasPrefix(c.q, "INSERT IGNORE INTO afv_task_event") {
			t.Fatalf("批不合规: rows=%d", len(c.args)/RowArgs)
		}
	}
}

// 关停冲刷：Close 把队列里的写完，之后的 Write 被丢弃计数。
func TestCloseFlushesThenDrops(t *testing.T) {
	db := &fakeDB{}
	w := New(db)
	for i := 0; i < 30; i++ {
		w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "a", Seq: i})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.rows()) != 30 {
		t.Fatalf("Close 应冲刷完，rows=%d", len(db.rows()))
	}
	w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, StepPath: "late"})
	if w.Dropped() != 1 {
		t.Fatalf("关闭后的写入应丢弃，dropped=%d", w.Dropped())
	}
	_ = w.Close(ctx) // 可重复
}

// panic 兜底：写入器自身 / 属性里的 typed-nil Stringer 都不会 panic。
type nilStr struct{}

func (*nilStr) String() string { return "x" }

type panicStr struct{}

func (panicStr) String() string { panic("boom") }

func TestNoPanicOnTypedNilStringerAndPanickyStringer(t *testing.T) {
	var np *nilStr
	if got := CleanAttrs(Attrs{"a": np, "b": 1}); got["b"] != 1 || got["a"] != nil {
		t.Fatalf("typed-nil 应被跳过: %v", got)
	}
	w := New(&fakeDB{})
	w.Write(context.Background(), Event{TaskID: "x", Kind: KindMark, Attrs: Attrs{"p": panicStr{}}}) // 不应 panic
	w.Mark(context.Background(), "x", "s", "m", 0, Attrs{"p": panicStr{}})
	w.Start(context.Background(), "x", "s", 0, Attrs{"p": panicStr{}}).OK(context.Background(), Attrs{"p": panicStr{}})
	w.Finish(context.Background(), "x", "s", 0, StatusOK, Fail{}, Attrs{"p": panicStr{}})
	w.LinkGatewayCall(context.Background(), "x", "s", "c", 0, Attrs{"p": panicStr{}})
	flush(t, w)
}

// dedupe 后缀：同一 attempt 下不同结局各占一条；默认 dedupe 不变。
func TestDedupeSuffixDistinguishesOutcomes(t *testing.T) {
	db := &fakeDB{}
	w := New(db)
	ctx := context.Background()
	w.Finish(ctx, "t", "go.x/q", 0, StatusFailed, Fail{}, nil, WithDedupeSuffix("failed"))
	w.Finish(ctx, "t", "go.x/q", 0, StatusOK, Fail{}, nil, WithDedupeSuffix("ok"))
	w.Finish(ctx, "t", "go.x/q", 0, StatusOK, Fail{}, nil)
	flush(t, w)
	r := db.rows()
	if len(r) != 3 || str(r[0][aDedupe]) != "span_end:go.x/q:0:failed" || str(r[1][aDedupe]) != "span_end:go.x/q:0:ok" || str(r[2][aDedupe]) != "span_end:go.x/q:0" {
		t.Fatalf("%v", r)
	}
}

func TestSpanStartEndAndLink(t *testing.T) {
	db := &fakeDB{}
	w := New(db)
	ctx := context.Background()
	sp := w.Start(ctx, "t1", "go.canvas/gateway_call/0", 1, Attrs{"operation": "image.generate"}, WithExternal(ExternalGatewayCall, "call_1"))
	sp.OK(ctx, nil)
	sp.OK(ctx, nil) // 只写一次
	w.LinkGatewayCall(ctx, "t1", "go.canvas/gateway_call/0", "call_1", 1, nil)
	flush(t, w)
	rows := db.rows()
	if len(rows) != 3 {
		t.Fatalf("rows=%d", len(rows))
	}
	if str(rows[0][aKind]) != "span_start" || str(rows[1][aKind]) != "span_end" || str(rows[1][aStatus]) != "ok" {
		t.Fatal("span 顺序 / 状态不对")
	}
	l := rows[2]
	if str(l[aKind]) != "gateway_call_linked" || str(l[aExtType]) != "gateway_call" || str(l[aExtRef]) != "call_1" ||
		str(l[aDedupe]) != "gateway_call_linked:go.canvas/gateway_call/0:call_1" {
		t.Fatalf("linked 行: %v", l)
	}
}
