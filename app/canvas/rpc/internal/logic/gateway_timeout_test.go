package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"

	"github.com/DATA-DOG/go-sqlmock"
)

// runningCall 未终态调用；accepted=true 时带一次已被供应商受理的尝试（accepted_at + POLLING）。
func runningCall(id string, accepted bool) []byte {
	c := aigateway.Call{ID: id, Status: aigateway.StatusRunning, Phase: "SENDING", RunNo: 1}
	if accepted {
		at := time.Now().Add(-time.Minute)
		c.Phase = "POLLING"
		c.Attempts = []aigateway.Attempt{{Seq: 1, RunNo: 1, State: "POLLING", AcceptedAt: &at, Charged: "UNKNOWN"}}
	} else {
		c.Attempts = []aigateway.Attempt{{Seq: 1, RunNo: 1, State: "SENDING"}}
	}
	b, _ := json.Marshal(c)
	return b
}

// timeoutGateway batchGet 按 view 回各调用；记录 cancel。
func timeoutGateway(t *testing.T, view func(id string) []byte) (*aigateway.Client, *atomic.Int32, *recorder) {
	var cancels atomic.Int32
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			cancels.Add(1)
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/calls/"), "/cancel")
			_, _ = w.Write(callJSON(id, aigateway.StatusCancelled, ""))
		case r.URL.Path == "/v1/calls:batchGet":
			var req struct{ IDs []string }
			_ = json.Unmarshal(body, &req)
			var items []string
			for _, id := range req.IDs {
				items = append(items, string(view(id)))
			}
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
		default:
			w.WriteHeader(404)
		}
	})
	return aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"}), &cancels, rec
}

func hardCapCore(t *testing.T) (*core, sqlmock.Sqlmock) {
	c, mock := mockCore(t)
	c.s.Config.Timeouts.Image, c.s.Config.Timeouts.ImageHardCap = 300, 900
	return c, mock
}

func timedRun(ageSec int, ref string) *model.NodeRun {
	run := gatewayRun(5, model.RunRunning)
	run.BillingStatus = model.BillHeld
	run.CreatedAt = time.Now().Add(-time.Duration(ageSec) * time.Second)
	run.BackendRef.String, run.BackendRef.Valid = ref, true
	return run
}

// 软时限已过（创建后 400s，软时限 300s），但调用已被供应商受理：不取消、不退款，继续等；之后出图照常成功收尾。
func TestAcceptedCallPastSoftTimeoutKeepsWaiting(t *testing.T) {
	c, mock := hardCapCore(t)
	var done atomic.Bool
	gw, cancels, _ := timeoutGateway(t, func(id string) []byte {
		if done.Load() {
			return callJSON(id, aigateway.StatusSucceeded, "https://gw/late.png")
		}
		return runningCall(id, true)
	})
	c.s.Gateway = gw
	run := timedRun(400, callA)
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning || cancels.Load() != 0 {
		t.Fatalf("已受理的调用软时限过后不应失败 / 取消: status=%d cancels=%d", run.Status, cancels.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	done.Store(true)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*late.png.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	run.BillingStatus = model.BillNone
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunSucceeded || cancels.Load() != 0 {
		t.Fatalf("延长等待后出图应成功收尾: status=%d cancels=%d", run.Status, cancels.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 过了硬上限：即便已受理也失败退款（CAS 一次）并取消调用。
func TestAcceptedCallHardCapFailsRefundsCancels(t *testing.T) {
	c, mock := hardCapCore(t)
	c.s.Billing = billingOn(t)
	gw, cancels, _ := timeoutGateway(t, func(id string) []byte { return runningCall(id, true) })
	c.s.Gateway = gw
	run := timedRun(901, callA)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`billing_status` = 'refunded'.*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunFailed || run.BillingStatus != model.BillRefunded || cancels.Load() != 1 {
		t.Fatalf("硬上限应失败退款并取消: status=%d bill=%s cancels=%d", run.Status, run.BillingStatus, cancels.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 软时限到了、调用还没被供应商受理（仍在上传参考图 / 发送中）：与改造前一致，失败退款并取消。
func TestUnacceptedCallAtSoftTimeoutFailsAsBefore(t *testing.T) {
	c, mock := hardCapCore(t)
	c.s.Billing = billingOn(t)
	gw, cancels, _ := timeoutGateway(t, func(id string) []byte { return runningCall(id, false) })
	c.s.Gateway = gw
	run := timedRun(301, callA)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`billing_status` = 'refunded'.*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunFailed || cancels.Load() != 1 {
		t.Fatalf("未受理的调用软时限到应失败并取消: status=%d cancels=%d", run.Status, cancels.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// count=2：一张已受理、一张未受理。软时限过后只取消未受理的那张，run 继续等已受理的。
func TestMixedCallsCancelOnlyUnaccepted(t *testing.T) {
	c, mock := hardCapCore(t)
	gw, cancels, rec := timeoutGateway(t, func(id string) []byte { return runningCall(id, id == callA) })
	c.s.Gateway = gw
	run := timedRun(400, callA+","+callB)
	run.RequestedCount = 2
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning || cancels.Load() != 1 || !rec.has("/v1/calls/"+callB+"/cancel") || rec.has("/v1/calls/"+callA+"/cancel") {
		t.Fatalf("只应取消未受理的 %s: status=%d paths=%v", callB, run.Status, rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 软时限过后调用状态拿不到（batchGet 失败）：不在不知道是否已受理时取消，等下一轮；硬上限兜底。
func TestUnknownCallStatePastSoftTimeoutWaits(t *testing.T) {
	c, mock := hardCapCore(t)
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { w.WriteHeader(503) })
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	run := timedRun(400, callA)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.pollGateway(ctx, []*model.NodeRun{run})
	if run.Status != model.RunRunning || rec.has("/v1/calls/"+callA+"/cancel") {
		t.Fatalf("状态未知时不应失败 / 取消: status=%d paths=%v", run.Status, rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 提交给网关的 deadline_at ≥ 硬上限（不让网关先于我们判超时）。
func TestGatewayDeadlineAtCoversHardCap(t *testing.T) {
	c, _ := hardCapCore(t)
	run := gatewayRun(5, model.RunQueued)
	before := time.Now()
	reqs, err := c.gatewayRequests(context.Background(), run, imageSpec)
	if err != nil || len(reqs) == 0 {
		t.Fatalf("构造请求失败: %v", err)
	}
	for _, r := range reqs {
		d := r.Constraints.DeadlineAt
		if d == nil || d.Before(before.Add(900*time.Second)) {
			t.Fatalf("deadline_at 应 ≥ 提交时刻 + 硬上限 900s: %v", d)
		}
	}
	// 硬上限没配（或小于软时限）时按软时限
	c.s.Config.Timeouts.ImageHardCap = 0
	if got := c.hardCap(model.KindImage); got != 300*time.Second {
		t.Fatalf("硬上限缺省应等于软时限: %s", got)
	}
}

// count=2 过了硬上限：一张已出图、一张已受理仍在跑 → 不整单退款丢图，取消在跑的那张、等它到终态按成功张数收尾；
// 宽限期过后仍不到终态才整单失败退款。
func TestHardCapWithDeliveredCancelsRestKeepsOutputs(t *testing.T) {
	c, mock := hardCapCore(t)
	gw, cancels, rec := timeoutGateway(t, func(id string) []byte {
		if id == callA {
			return callJSON(id, aigateway.StatusSucceeded, "https://gw/a.png")
		}
		return runningCall(id, true)
	})
	c.s.Gateway = gw
	run := timedRun(901, callA+","+callB)
	run.RequestedCount = 2
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning || cancels.Load() != 1 || !rec.has("/v1/calls/"+callB+"/cancel") || rec.has("/v1/calls/"+callA+"/cancel") {
		t.Fatalf("硬上限已有成品：只取消在跑的 %s、不失败: status=%d paths=%v", callB, run.Status, rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// 宽限期过后仍未终态 → 整单失败退款
	ids := []string{callA, callB}
	ok := &aigateway.Call{ID: callA, Status: aigateway.StatusSucceeded}
	running := &aigateway.Call{ID: callB, Status: aigateway.StatusRunning, Phase: "POLLING"}
	late := timedRun(901+int(hardCapGrace/time.Second)+1, callA+","+callB)
	if v := c.gatewayTimeout(late, ids, []*aigateway.Call{ok, running}); !v.fail {
		t.Fatalf("宽限期过后应整单失败: %+v", v)
	}
	// 硬上限且没有成品 → 照旧整单失败
	if v := c.gatewayTimeout(timedRun(901, callA+","+callB), ids, []*aigateway.Call{running, running}); !v.fail {
		t.Fatalf("没有成品的硬上限应整单失败: %+v", v)
	}
}

// callAccepted 判据：money_at_risk / accepted_at / 受理后状态 / 调用阶段；上一轮运行的受理不算本轮。
func TestCallAccepted(t *testing.T) {
	at := time.Now()
	cases := []struct {
		c    *aigateway.Call
		want bool
	}{
		{&aigateway.Call{Status: "RUNNING", MoneyAtRisk: true}, true},
		{&aigateway.Call{Status: "RUNNING", Phase: "POLLING"}, true},
		{&aigateway.Call{Status: "RUNNING", RunNo: 1, Attempts: []aigateway.Attempt{{RunNo: 1, State: "ACCEPTED"}}}, true},
		{&aigateway.Call{Status: "RUNNING", RunNo: 1, Attempts: []aigateway.Attempt{{RunNo: 1, State: "SENDING", AcceptedAt: &at}}}, true},
		{&aigateway.Call{Status: "RUNNING", RunNo: 2, Attempts: []aigateway.Attempt{{RunNo: 1, State: "ACCEPTED"}, {RunNo: 2, State: "SENDING"}}}, false},
		{&aigateway.Call{Status: "PENDING", Phase: "ROUTING"}, false},
		// 上一个部署受理后失败、网关正换部署（新尝试未受理）：已结束的受理不算，money_at_risk 有明细时不单独作数
		{&aigateway.Call{Status: "RUNNING", Phase: "WAITING_CAPACITY", RunNo: 1, MoneyAtRisk: true, Attempts: []aigateway.Attempt{
			{RunNo: 1, State: "FAILED", AcceptedAt: &at}, {RunNo: 1, State: "PREPARED"}}}, false},
		{&aigateway.Call{Status: "RUNNING", RunNo: 1, MoneyAtRisk: true, Attempts: []aigateway.Attempt{{RunNo: 1, State: "UNKNOWN", AcceptedAt: &at}}}, false},
		{nil, false},
	}
	for i, x := range cases {
		if got := callAccepted(x.c); got != x.want {
			t.Fatalf("case %d: got %v want %v", i, got, x.want)
		}
	}
}

// 长轮询主路径：软时限已过但调用已受理 → 不取消，接着长轮询直到出图成功。
func TestWatchRunAcceptedPastSoftTimeout(t *testing.T) {
	fastWatch(t)
	c, mock := hardCapCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	var gets, cancels atomic.Int32
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.URL.Path == "/v1/calls/"+callA {
			if gets.Add(1) < 3 {
				_, _ = w.Write(runningCall(callA, true))
				return
			}
			_, _ = w.Write(callJSON(callA, aigateway.StatusSucceeded, "https://gw/late.png"))
			return
		}
		if r.URL.Path == "/v1/calls:batchGet" {
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, runningCall(callA, true))
			return
		}
		cancels.Add(1)
		w.WriteHeader(404)
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	created := time.Now().Add(-400 * time.Second)
	for i := 0; i < 3; i++ {
		mock.ExpectQuery("WHERE \\(r.id = 5\\) LIMIT 1").WillReturnRows(sqlmock.NewRows(runRowCols).
			AddRow(5, "cv_1", "m", 7, "image", "gateway", model.RunRunning, model.BillNone, 1, callA, "cv_t", `{"model":"Banana"}`, created))
	}
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*late.png.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.watchRun(context.Background(), 5)
	if cancels.Load() != 0 {
		t.Fatalf("已受理的调用不应被取消")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
