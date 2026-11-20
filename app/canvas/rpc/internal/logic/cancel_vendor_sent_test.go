package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

const cancelRefusedBody = `{"error":{"code":"CANCEL_NOT_ALLOWED","check":"cancel.vendor_sent","message":"sent","call_id":"%s"}}`

// cancelGateway batchGet 按 view 回各调用；/cancel 按 refuse 决定回 409 CANCEL_NOT_ALLOWED 还是 CANCELLED。
func cancelGateway(t *testing.T, c *core, view func(id string) []byte, refuse func(id string) bool) *recorder {
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/calls/"), "/cancel")
			if refuse(id) {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, cancelRefusedBody, id)
				return
			}
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
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	return rec
}

// waitingCall 未发送（等容量）的调用。
func waitingCall(id string) []byte {
	b, _ := json.Marshal(aigateway.Call{ID: id, Status: aigateway.StatusRunning, Phase: "WAITING_CAPACITY", RunNo: 1})
	return b
}

func expectHeldRunningRun(mock sqlmock.Sqlmock, ref string, count int) {
	mock.ExpectQuery("WHERE \\(r.id = 5\\) LIMIT 1").WillReturnRows(sqlmock.NewRows(append(runRowCols, "billing_request_id", "points")).
		AddRow(5, "cv_1", "m", 7, "image", "gateway", model.RunRunning, model.BillHeld, count, ref, "cv_t", `{"model":"Banana"}`, time.Now(), "canvas-cv_t", 40))
}

func isVendorSentRefusal(err error) bool {
	code, msg, _ := xerr.Parse(err)
	return code == xerr.Conflict && msg == msgCancelVendorSent && xerr.ReasonOf(err) == xerr.ReasonCancelVendorSent
}

// 供应商已受理：预检即拒绝，一条都不取消，run 不结束、不退款（没有任何 UPDATE）。
func TestCancelRunVendorAcceptedRefusedNoRefund(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	rec := cancelGateway(t, c, func(id string) []byte { return runningCall(id, id == callA) }, func(string) bool { return false })
	expectHeldRunningRun(mock, callA+","+callB, 2)
	_, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if !isVendorSentRefusal(err) {
		t.Fatalf("已受理应 4002 + CANCEL_VENDOR_SENT: %v", err)
	}
	if rec.has("/v1/calls/"+callA+"/cancel") || rec.has("/v1/calls/"+callB+"/cancel") {
		t.Fatalf("预检拒绝时一条都不能取消（防止取消一半）: %v", rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// vendor_accepted=true（没有尝试明细）同样拒绝；已出图（SUCCEEDED）的也拒绝，交给轮询器交付结算。
func TestCancelRunVendorAcceptedFlagAndSucceededRefused(t *testing.T) {
	for name, view := range map[string]func(string) []byte{
		"vendor_accepted": func(id string) []byte {
			b, _ := json.Marshal(aigateway.Call{ID: id, Status: aigateway.StatusRunning, Phase: "ROUTING", VendorAccepted: true})
			return b
		},
		"succeeded": func(id string) []byte { return callJSON(id, aigateway.StatusSucceeded, "https://c/1.png") },
		"sending": func(id string) []byte {
			b, _ := json.Marshal(aigateway.Call{ID: id, Status: aigateway.StatusRunning, Phase: "SENDING", RunNo: 1})
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, mock := mockCore(t)
			rec := cancelGateway(t, c, view, func(string) bool { return false })
			expectHeldRunningRun(mock, callA, 1)
			if _, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5}); !isVendorSentRefusal(err) {
				t.Fatalf("应拒绝: %v", err)
			}
			if rec.has("/v1/calls/" + callA + "/cancel") {
				t.Fatal("不应取消")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 还没发给供应商：照常先取消上游，再结束并退款。
func TestCancelRunUnsentCancelsThenRefunds(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	rec := cancelGateway(t, c, waitingCall, func(string) bool { return false })
	expectHeldRunningRun(mock, callA+","+callB, 2)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 4, .*`billing_status` = 'refunded'.*`error_code` = 'CANCELED'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	out, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if err != nil || out.Status != model.StatusText(model.RunCanceled) {
		t.Fatalf("未发送应取消成功: %+v %v", out, err)
	}
	if !rec.has("/v1/calls/"+callA+"/cancel") || !rec.has("/v1/calls/"+callB+"/cancel") {
		t.Fatalf("应取消全部调用: %v", rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 竞态：预检时都没发送，取消时第二条刚被发出去（网关 409）。第一条已取消（未发送，零成本），run 保持运行中、不退款，
// 拒绝用户取消，交给轮询器按部分成功收尾。
func TestCancelRunRaceRefusedKeepsRunning(t *testing.T) {
	c, mock := mockCore(t)
	rec := cancelGateway(t, c, waitingCall, func(id string) bool { return id == callB })
	expectHeldRunningRun(mock, callA+","+callB, 2)
	if _, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5}); !isVendorSentRefusal(err) {
		t.Fatalf("竞态被拒应同样拒绝: %v", err)
	}
	if !rec.has("/v1/calls/"+callA+"/cancel") || !rec.has("/v1/calls/"+callB+"/cancel") {
		t.Fatalf("两条都应尝试取消: %v", rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有 UPDATE：不结束、不退款
		t.Fatal(err)
	}
	// 轮询器随后看到：A 已取消、B 成功 → 按成功 1 张收尾（不是取消 / 全退）
	o := ClassifyGateway(model.KindImage, []*aigateway.Call{
		{ID: callA, Status: aigateway.StatusCancelled},
		{ID: callB, Status: aigateway.StatusSucceeded, Result: &aigateway.Result{Outputs: []aigateway.OutputItem{{Status: aigateway.StatusSucceeded, Media: &aigateway.MediaOut{URL: "https://c/1.png"}}}}},
	})
	if !o.done || !o.ok || o.actual != 1 {
		t.Fatalf("部分成功应按成功张数收尾: %+v", o)
	}
}

// 预检读不到调用（网关不可用）：不取消、不结束、不退款，回可重试错误。
func TestCancelRunPrecheckUnavailableKeepsRunning(t *testing.T) {
	c, mock := mockCore(t)
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { w.WriteHeader(503) })
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	expectHeldRunningRun(mock, callA, 1)
	_, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if code, _, _ := xerr.Parse(err); code != xerr.ThirdPartyError {
		t.Fatalf("应回可重试错误: %v", err)
	}
	if rec.has("/v1/calls/" + callA + "/cancel") {
		t.Fatal("不知道是否已发送时不取消")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// statefulGateway 每条调用一个可变视图；/cancel 对 refuse 的回 409，否则把视图改成 CANCELLED。
type statefulGateway struct {
	mu     sync.Mutex
	view   map[string][]byte
	refuse map[string]bool
	n      map[string]int // 每条调用收到的 cancel 次数
}

func (g *statefulGateway) set(id string, b []byte) {
	g.mu.Lock()
	g.view[id] = b
	g.mu.Unlock()
}

func (g *statefulGateway) cancels(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n[id]
}

func newStatefulGateway(t *testing.T, c *core, view map[string][]byte, refuse map[string]bool) *statefulGateway {
	g := &statefulGateway{view: view, refuse: refuse, n: map[string]int{}}
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/calls/"), "/cancel")
			g.n[id]++
			if g.refuse[id] {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, cancelRefusedBody, id)
				return
			}
			g.view[id] = callJSON(id, aigateway.StatusCancelled, "")
			_, _ = w.Write(g.view[id])
		case r.URL.Path == "/v1/calls:batchGet":
			var req struct{ IDs []string }
			_ = json.Unmarshal(body, &req)
			var items []string
			for _, id := range req.IDs {
				items = append(items, string(g.view[id]))
			}
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
		default:
			w.WriteHeader(404)
		}
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	return g
}

// 超时（硬上限）：已受理的 A 取消被拒、未发送的 B 照常取消；run 不失败、不退款，等 A。A 随后成功 → 按成功 1 张交付。
func TestTimeoutRefusedThenSucceedsDeliversPartial(t *testing.T) {
	c, mock := hardCapCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	g := newStatefulGateway(t, c, map[string][]byte{callA: runningCall(callA, true), callB: waitingCall(callB)}, map[string]bool{callA: true})
	run := timedRun(901, callA+","+callB)
	run.RequestedCount = 2
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning || g.cancels(callA) != 1 || g.cancels(callB) != 1 {
		t.Fatalf("被拒应继续等、未发送的照常取消: status=%d A=%d B=%d", run.Status, g.cancels(callA), g.cancels(callB))
	}
	// 等待期内再判一轮：仍未终态的 A 再取消一次（仍被拒就继续等），已 CANCELLED 的 B 不再重复发 Cancel
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning || g.cancels(callA) != 2 || g.cancels(callB) != 1 {
		t.Fatalf("已取消的不应重复取消: status=%d A=%d B=%d", run.Status, g.cancels(callA), g.cancels(callB))
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有任何 UPDATE：不失败、不退款
		t.Fatal(err)
	}
	g.set(callA, callJSON(callA, aigateway.StatusSucceeded, "https://gw/a.png"))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*`candidates` = .*a.png.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` .*`billing_status` = 'settled'.*WHERE \\(id = 5\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunSucceeded || run.BillingStatus != model.BillSettled {
		t.Fatalf("A 成功后应按成功张数交付结算: status=%d bill=%s", run.Status, run.BillingStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 超时：被拒的调用随后失败 → 按失败退款（不是超时、也不是取消）。
func TestTimeoutRefusedThenFailsRefunds(t *testing.T) {
	c, mock := hardCapCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	g := newStatefulGateway(t, c, map[string][]byte{callA: runningCall(callA, true)}, map[string]bool{callA: true})
	run := timedRun(301, callA) // 软时限刚过、调用读成未受理也一样：先取消，被拒就等
	g.set(callA, waitingCall(callA))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning || g.cancels(callA) != 1 {
		t.Fatalf("软时限取消被拒应继续等: status=%d cancels=%d", run.Status, g.cancels(callA))
	}
	failed, _ := json.Marshal(aigateway.Call{ID: callA, Status: aigateway.StatusFailed, Failure: &aigateway.Failure{Class: "VENDOR_FAILED", Code: "x"}})
	g.set(callA, failed)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`billing_status` = 'refunded'.*`error_code` = 'UPSTREAM_FAILED'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunFailed || run.BillingStatus != model.BillRefunded {
		t.Fatalf("被拒调用失败后应失败退款: status=%d bill=%s", run.Status, run.BillingStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 等被拒调用超过上限（创建 + 硬上限 + VendorSentWait）仍未终态 → 按超时失败退款。
func TestTimeoutRefusedWaitExceededFails(t *testing.T) {
	c, mock := hardCapCore(t)
	c.s.Config.Timeouts.VendorSentWait = 600
	c.s.Billing = billingOn(t)
	g := newStatefulGateway(t, c, map[string][]byte{callA: runningCall(callA, true)}, map[string]bool{callA: true})
	run := timedRun(900+600-5, callA) // 上限内：继续等
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunRunning {
		t.Fatalf("上限内应继续等: %d", run.Status)
	}
	run = timedRun(900+600+5, callA)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`billing_status` = 'refunded'.*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{run})
	if run.Status != model.RunFailed || g.cancels(callA) != 2 {
		t.Fatalf("超过上限应超时失败: status=%d cancels=%d", run.Status, g.cancels(callA))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 运营强制失败不变：先结束并退款，再取消；网关拒绝只打日志。
func TestForceFailRefusedUnchanged(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	g := newStatefulGateway(t, c, map[string][]byte{callA: runningCall(callA, true)}, map[string]bool{callA: true})
	expectHeldRunningRun(mock, callA, 1)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`billing_status` = 'refunded'.*`error_code` = 'ADMIN_FORCE_FAIL'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := c.AdminForceFailRun(context.Background(), &canvas.AdminRunReq{Id: 5, AdminId: 1}); err != nil {
		t.Fatal(err)
	}
	if g.cancels(callA) != 1 {
		t.Fatalf("应尝试取消一次: %d", g.cancels(callA))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 派发期间 run 已被用户取消（已退款），本次拿到的调用已发给供应商、网关拒绝取消：只记 ERROR（成本待核对），不改 run、不崩。
func TestUnrecordedCallRefusedAfterCancelLogsOnly(t *testing.T) {
	c, mock := mockCore(t)
	g := newStatefulGateway(t, c, map[string][]byte{callA: runningCall(callA, true)}, map[string]bool{callA: true})
	mock.ExpectQuery("WHERE \\(r.id = 5\\) LIMIT 1").WillReturnRows(sqlmock.NewRows(runRowCols).
		AddRow(5, "cv_1", "m", 7, "image", "gateway", model.RunCanceled, model.BillRefunded, 1, nil, "cv_t", `{"model":"Banana"}`, time.Now()))
	c.cancelUnrecordedCalls(context.Background(), 5, []string{callA}, "提交期间已结束")
	if g.cancels(callA) != 1 {
		t.Fatalf("应尝试取消一次: %d", g.cancels(callA))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
