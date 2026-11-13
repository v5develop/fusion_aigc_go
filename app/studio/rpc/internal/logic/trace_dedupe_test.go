package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskevent"
)

func sdedupes(rows []stRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.dedupe)
	}
	return out
}

func sdistinct(ss []string) bool {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

// 提交网关调用：第一次失败、第二次（同一 attempt）成功，失败行自带序号，成功的 span_end 不被吞；
// 失败说明只带码，不带网关原始响应体。
func TestStudioTraceSubmitFailedThenOKBothRecorded(t *testing.T) {
	f, mock, rec := traceFlow(t, onBill())
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "bad_input", "message": "prompt 'MYSECRETPROMPT' https://x.com/a?sig=1"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "call_aaaaaaaaaa08", "status": "QUEUED"})
	}))
	defer srv.Close()
	f.gw = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task`")).WillReturnResult(ok1)
	tk := runningTask()
	tk.GatewayCallID = sql.NullString{}
	tk.ModelGroup = "img.gpt-image-2"
	tk.PromptText = sql.NullString{String: "p", Valid: true}
	b := *batch9
	b.Payload = json.RawMessage(`{}`)
	if _, err := f.submit(context.Background(), tk, &b, false); err == nil {
		t.Fatal("第一次应失败")
	}
	time.Sleep(3 * time.Millisecond)
	if _, err := f.submit(context.Background(), tk, &b, false); err != nil {
		t.Fatal(err)
	}
	ends := rec.find(taskevent.KindSpanEnd, stCall)
	if len(ends) != 2 || ends[0].status != taskevent.StatusFailed || ends[1].status != taskevent.StatusOK || !sdistinct(sdedupes(ends)) {
		t.Fatalf("应记 failed + ok 且 dedupe 不同: %+v", ends)
	}
	if len(rec.find(taskevent.KindSpanStart, stCall)) != 2 { // start 每次都发，dedupe 相同只落一行（INSERT IGNORE）
		t.Fatal("start 应入队两次")
	}
	for _, r := range rec.rows {
		if r.step == stCall && (strings.Contains(r.payload, "MYSECRETPROMPT") || strings.Contains(r.code, "MYSECRET")) {
			t.Fatalf("事件带了原始响应体: %+v", r)
		}
	}
}

// 排队区间收尾：失败后又顺利派发（或反过来）两次结局各记一条，status 进 dedupe。
func TestStudioTraceQueueDoneStatusInDedupe(t *testing.T) {
	f, _, rec := traceFlow(t, onBill())
	tk := runningTask()
	tk.CreatedAt = time.Now().Add(-time.Minute)
	f.evQueueDone(context.Background(), tk, taskevent.StatusFailed, taskevent.Fail{Class: "LOCAL", Code: "QUEUED_FAILED", Check: "x", Message: "m"})
	f.evQueueDone(context.Background(), tk, taskevent.StatusOK, taskevent.Fail{})
	f.evQueueDone(context.Background(), tk, taskevent.StatusOK, taskevent.Fail{})
	ends := rec.find(taskevent.KindSpanEnd, stQueue)
	if len(ends) != 3 {
		t.Fatalf("三次入队: %+v", ends)
	}
	if d := sdedupes(ends); d[0] == d[1] || d[1] != d[2] {
		t.Fatalf("failed / ok 的 dedupe 不同，重复 ok 相同（INSERT IGNORE 去重）: %v", d)
	}
}

// 取消排队任务：evCancel 不声称退款已发生；退款区间由 bill.Fail 之后的 evRefundRequested 写，result=requested。
func TestStudioTraceCancelDoesNotClaimRefund(t *testing.T) {
	f, _, rec := traceFlow(t, onBill())
	d := &deps{ev: f.ev, cfg: f.cfg}
	tk := runningTask()
	tk.CreatedAt = time.Now().Add(-time.Minute)
	d.evCancel(context.Background(), tk, true)
	if got := rec.find(taskevent.KindSpanEnd, stRefund); len(got) != 0 {
		t.Fatalf("取消本身不应写退款区间: %+v", got)
	}
	d.evRefundRequested(context.Background(), tk, "用户取消")
	got := rec.find(taskevent.KindSpanEnd, stRefund)
	if len(got) != 1 || got[0].status != taskevent.StatusOK {
		t.Fatalf("%+v", got)
	}
	var p struct{ Attrs map[string]any }
	_ = json.Unmarshal([]byte(got[0].payload), &p)
	if p.Attrs["result"] != "requested" {
		t.Fatalf("attrs=%v", p.Attrs)
	}
}
