package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/taskevent"

	"github.com/DATA-DOG/go-sqlmock"
)

// evRow afv_task_event 里写进去的一行（参数下标见 taskevent.insertSQL）。
type evRow struct{ task, step, kind, status, class, code, extType, extRef, source, dedupe, payload string }

type evRec struct {
	mu   sync.Mutex
	rows []evRow
	w    *taskevent.Writer
}

func (r *evRec) ExecCtx(_ context.Context, q string, a ...any) (sql.Result, error) {
	s := func(v any) string {
		if ns, ok := v.(sql.NullString); ok {
			return ns.String
		}
		if x, ok := v.(string); ok {
			return x
		}
		return ""
	}
	if !strings.HasPrefix(q, "INSERT IGNORE INTO afv_task_event") {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i+taskevent.RowArgs <= len(a); i += taskevent.RowArgs { // 异步写入器是多行 INSERT
		a := a[i : i+taskevent.RowArgs]
		r.rows = append(r.rows, evRow{task: s(a[0]), step: s(a[2]), kind: s(a[3]), status: s(a[4]), class: s(a[5]), code: s(a[6]),
			extType: s(a[11]), extRef: s(a[12]), source: s(a[14]), dedupe: s(a[16]), payload: s(a[17])})
	}
	return nil, nil
}

// flush 等异步写入器把已入队的事件落完。
func (r *evRec) flush() {
	if r.w != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = r.w.Flush(ctx)
	}
}

func (r *evRec) find(kind, step string) []evRow {
	r.flush()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []evRow
	for _, x := range r.rows {
		if x.kind == kind && x.step == step {
			out = append(out, x)
		}
	}
	return out
}

func withEvents(c *core) *evRec {
	rec := &evRec{}
	c.s.Events = taskevent.New(rec)
	rec.w = c.s.Events
	return rec
}

func attrsOf(t *testing.T, payload string) map[string]any {
	t.Helper()
	var p struct {
		Attrs map[string]any `json:"attrs"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("payload 不是合法 JSON: %q: %v", payload, err)
	}
	return p.Attrs
}

// 过了硬上限：go.canvas/timeout 的 span_end 状态为 timeout，reason=hard_cap，带软 / 硬上限与耗时；wait 区间同样以 timeout 收尾；
// task_id 是任务中心的编号 taskcenter.Ref，source=go；失败退款留下 refund 区间。
func TestTraceHardCapTimeoutSpan(t *testing.T) {
	c, mock := hardCapCore(t)
	ev := withEvents(c)
	c.s.Billing = billingOn(t)
	gw, _, _ := timeoutGateway(t, func(id string) []byte { return runningCall(id, true) })
	c.s.Gateway = gw
	run := timedRun(901, callA)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})

	end := ev.find(taskevent.KindSpanEnd, stTimeout)
	if len(end) != 1 || end[0].status != taskevent.StatusTimeout || end[0].code != "TIMEOUT" || end[0].class != "TIMEOUT" {
		t.Fatalf("应有一条 status=timeout 的 timeout span_end: %+v", end)
	}
	if want := taskcenter.Ref("cv", 5); end[0].task != want || end[0].source != "go" {
		t.Fatalf("task_id 应为任务中心编号 %s，source=go: %+v", want, end[0])
	}
	a := attrsOf(t, end[0].payload)
	if a["reason"] != "hard_cap" || a["decision"] != "fail" || a["hard_cap_s"] != float64(900) || a["soft_cap_s"] != float64(300) {
		t.Fatalf("reason 属性不对: %v", a)
	}
	if len(ev.find(taskevent.KindSpanStart, stTimeout)) != 1 {
		t.Fatal("缺 timeout 的 span_start")
	}
	if w := ev.find(taskevent.KindSpanEnd, stWait); len(w) != 1 || w[0].status != taskevent.StatusTimeout {
		t.Fatalf("wait 区间应以 timeout 收尾: %+v", w)
	}
	if r := ev.find(taskevent.KindSpanEnd, stRefund); len(r) != 1 || r[0].status != taskevent.StatusOK {
		t.Fatalf("应有 refund 区间: %+v", r)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 软时限到、没有调用被受理：reason=soft_cap_none_accepted。
func TestTraceSoftCapNoneAcceptedReason(t *testing.T) {
	c, mock := hardCapCore(t)
	ev := withEvents(c)
	c.s.Billing = billingOn(t)
	gw, _, _ := timeoutGateway(t, func(id string) []byte { return runningCall(id, false) })
	c.s.Gateway = gw
	run := timedRun(301, callA)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'TIMEOUT'.*").WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	end := ev.find(taskevent.KindSpanEnd, stTimeout)
	if len(end) != 1 || end[0].status != taskevent.StatusTimeout {
		t.Fatalf("%+v", end)
	}
	if a := attrsOf(t, end[0].payload); a["reason"] != "soft_cap_none_accepted" || a["accepted"] != false {
		t.Fatalf("attrs=%v", a)
	}
}

// 软时限过后有调用已受理：不超时，timeout 区间以 skipped 收尾，reason=soft_cap_accepted_extended（不是 timeout 状态）。
func TestTraceExtendedDecisionIsSkipped(t *testing.T) {
	c, _ := hardCapCore(t)
	ev := withEvents(c)
	gw, _, _ := timeoutGateway(t, func(id string) []byte { return runningCall(id, true) })
	c.s.Gateway = gw
	run := timedRun(400, callA)
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	end := ev.find(taskevent.KindSpanEnd, stTimeout)
	if len(end) != 1 || end[0].status != taskevent.StatusSkipped || end[0].code != "TIMEOUT_EXTENDED" {
		t.Fatalf("延长应记为 skipped: %+v", end)
	}
	if a := attrsOf(t, end[0].payload); a["reason"] != "soft_cap_accepted_extended" || a["decision"] != "extend" || a["accepted"] != true {
		t.Fatalf("attrs=%v", a)
	}
	if run.Status != model.RunRunning {
		t.Fatal("延长期间 run 应仍在运行")
	}
}

// 提交网关调用：每个调用创建后立刻直接写库 gateway_call_linked（external_ref=调用号），gateway_call/<i> 区间带调用号收尾；
// 提交请求带 X-Trace-Id = 任务中心编号；转运行中后 queue 以 ok 收尾、wait 开始。
func TestTraceGatewayCallLinkedAndTraceHeader(t *testing.T) {
	c, mock := mockCore(t)
	ev := withEvents(c)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	var mu sync.Mutex
	var hdr string
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		hdr = r.Header.Get("X-Trace-Id")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "call_aaaaaaaaaa07", "status": "QUEUED"})
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	run := gatewayRun(41, model.RunQueued)
	run.Request = json.RawMessage(`{"model":"Banana","prompt":"p","count":1}`)
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("SET `status` = 1, .*WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), run, sp, ""); err != nil {
		t.Fatal(err)
	}
	want := taskcenter.Ref("cv", 41)
	mu.Lock()
	got := hdr
	mu.Unlock()
	if got != want {
		t.Fatalf("X-Trace-Id = %q, want %q", got, want)
	}
	linked := ev.find(taskevent.KindGatewayCallLinked, "go.canvas/gateway_call/0")
	if len(linked) != 1 || linked[0].extType != "gateway_call" || linked[0].extRef != "call_aaaaaaaaaa07" || linked[0].task != want {
		t.Fatalf("gateway_call_linked: %+v", linked)
	}
	if !strings.HasSuffix(linked[0].dedupe, ":call_aaaaaaaaaa07") {
		t.Fatalf("dedupe=%s", linked[0].dedupe)
	}
	if e := ev.find(taskevent.KindSpanEnd, "go.canvas/gateway_call/0"); len(e) != 1 || e[0].status != "ok" || e[0].extRef != "call_aaaaaaaaaa07" {
		t.Fatalf("gateway_call span_end: %+v", e)
	}
	if q := ev.find(taskevent.KindSpanEnd, stQueue); len(q) != 1 || q[0].status != "ok" {
		t.Fatalf("queue 应以 ok 收尾: %+v", q)
	}
	if len(ev.find(taskevent.KindSpanStart, stWait)) != 1 || len(ev.find(taskevent.KindSpanEnd, stDispatch)) != 1 {
		t.Fatal("缺 wait 开始 / dispatch 结束")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
