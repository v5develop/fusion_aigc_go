package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/taskevent"

	"github.com/DATA-DOG/go-sqlmock"
)

type stRow struct{ task, step, kind, status, code, extRef, source, payload, dedupe string }

type stRec struct {
	mu   sync.Mutex
	rows []stRow
	w    *taskevent.Writer
}

func (r *stRec) ExecCtx(_ context.Context, q string, a ...any) (sql.Result, error) {
	s := func(v any) string {
		if ns, ok := v.(sql.NullString); ok {
			return ns.String
		}
		x, _ := v.(string)
		return x
	}
	if strings.HasPrefix(q, "INSERT IGNORE INTO afv_task_event") {
		r.mu.Lock()
		for i := 0; i+taskevent.RowArgs <= len(a); i += taskevent.RowArgs { // 异步写入器是多行 INSERT
			a := a[i : i+taskevent.RowArgs]
			r.rows = append(r.rows, stRow{task: s(a[0]), step: s(a[2]), kind: s(a[3]), status: s(a[4]), code: s(a[6]), extRef: s(a[12]), source: s(a[14]), payload: s(a[17]), dedupe: s(a[16])})
		}
		r.mu.Unlock()
	}
	return nil, nil
}

func (r *stRec) find(kind, step string) []stRow {
	if r.w != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = r.w.Flush(ctx)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []stRow
	for _, x := range r.rows {
		if x.kind == kind && x.step == step {
			out = append(out, x)
		}
	}
	return out
}

func traceFlow(t *testing.T, bill *fakeFlowBill) (*flow, sqlmock.Sqlmock, *stRec) {
	f, mock, _ := newTestFlow(t, bill)
	rec := &stRec{}
	f.ev = taskevent.New(rec)
	rec.w = f.ev
	f.cfg = config.Config{}
	f.cfg.TaskIDPrefix.ImageGen = "simg"
	return f, mock, rec
}

// 生图超时置失败：go.studio/timeout 以 timeout 收尾（带 reason / 时限），wait 同样 timeout，退款留 refund 区间；
// task_id 是任务中心编号。
func TestStudioTraceTimeoutSpan(t *testing.T) {
	bill := onBill()
	bill.status = resvConfirmed
	f, mock, rec := traceFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	tk := runningTask()
	tk.StartedAt = sql.NullTime{Time: time.Now().Add(-10 * time.Minute), Valid: true}
	f.failTask(context.Background(), tk, batch9, reasonTimeout, true, "RUNNING")

	end := rec.find(taskevent.KindSpanEnd, stTimeout)
	if len(end) != 1 || end[0].status != taskevent.StatusTimeout || end[0].code != "TIMEOUT" {
		t.Fatalf("timeout span_end: %+v", end)
	}
	if want := taskcenter.Ref("simg", 1); end[0].task != want || end[0].source != "go" {
		t.Fatalf("task_id 应为 %s: %+v", want, end[0])
	}
	var p struct{ Attrs map[string]any }
	_ = json.Unmarshal([]byte(end[0].payload), &p)
	if p.Attrs["reason"] != "generate_timeout" || p.Attrs["decision"] != "fail" || p.Attrs["gateway_status"] != "RUNNING" {
		t.Fatalf("attrs=%v", p.Attrs)
	}
	if w := rec.find(taskevent.KindSpanEnd, stWait); len(w) != 1 || w[0].status != taskevent.StatusTimeout {
		t.Fatalf("wait: %+v", w)
	}
	if r := rec.find(taskevent.KindSpanEnd, stRefund); len(r) != 1 || r[0].status != taskevent.StatusOK {
		t.Fatalf("refund: %+v", r)
	}
	met(t, mock)
}

// 提交网关调用：带 X-Trace-Id（任务中心编号）、写 gateway_call_linked（external_ref=调用号）、wait 区间开始。
func TestStudioTraceSubmitLinkedAndHeader(t *testing.T) {
	f, mock, rec := traceFlow(t, onBill())
	var mu sync.Mutex
	var hdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hdr = r.Header.Get("X-Trace-Id")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "call_aaaaaaaaaa07", "status": "QUEUED"})
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
	call, err := f.submit(context.Background(), tk, &b, false)
	if err != nil {
		t.Fatal(err)
	}
	want := taskcenter.Ref("simg", 1)
	mu.Lock()
	got := hdr
	mu.Unlock()
	if got != want {
		t.Fatalf("X-Trace-Id=%q want %q", got, want)
	}
	linked := rec.find(taskevent.KindGatewayCallLinked, stCall)
	if len(linked) != 1 || linked[0].extRef != call.ID || linked[0].task != want {
		t.Fatalf("linked: %+v", linked)
	}
	if len(rec.find(taskevent.KindSpanStart, stWait)) != 1 {
		t.Fatal("缺 wait 开始")
	}
}
