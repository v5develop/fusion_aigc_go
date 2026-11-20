package logic

// 排队 / 提交中取消（Go_V1.0.61 cancel_request）：退款只有一个主人，且只在确认没发给供应商之后退。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"

	"github.com/DATA-DOG/go-sqlmock"
)

// crGateway 提交回 callA（排队）；batchGet 按 view；/cancel 按 cancel（返回 HTTP 状态：200 = 已取消，409 = 已发给供应商，503 = 不可用）。
type crGateway struct {
	mu      sync.Mutex
	view    func(id string) []byte
	cancel  func(id string) int
	lookup  int // GET /v1/calls?idempotency_key= 回的 HTTP 状态：200 = 有调用（callA），404 = 没建过单，其它 = 查询失败
	submits int // POST /v1/calls（新建 / 重放）
	lookups int
	cancels map[string]int
	paths   []string
}

func newCRGateway(t *testing.T, c *core, view func(string) []byte, cancel func(string) int) *crGateway {
	g := &crGateway{view: view, cancel: cancel, lookup: http.StatusOK, cancels: map[string]int{}}
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.paths = append(g.paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/calls/"), "/cancel")
			g.cancels[id]++
			switch code := g.cancel(id); code {
			case http.StatusConflict:
				w.WriteHeader(code)
				_, _ = fmt.Fprintf(w, cancelRefusedBody, id)
			case http.StatusOK:
				_, _ = w.Write(callJSON(id, aigateway.StatusCancelled, ""))
			default:
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"down"}}`))
			}
		case r.URL.Path == "/v1/calls:batchGet":
			var req struct{ IDs []string }
			_ = json.Unmarshal(body, &req)
			var items []string
			for _, id := range req.IDs {
				items = append(items, string(g.view(id)))
			}
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
		case r.URL.Path == "/v1/calls" && r.Method == http.MethodGet:
			g.lookups++
			switch g.lookup {
			case http.StatusOK:
				_, _ = w.Write(g.view(callA))
			case http.StatusNotFound:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","check":"call.exists","message":"no call"}}`))
			default:
				w.WriteHeader(g.lookup)
				_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"down"}}`))
			}
		case r.URL.Path == "/v1/calls":
			g.submits++
			_, _ = w.Write(callJSON(callA, aigateway.StatusPending, ""))
		default:
			w.WriteHeader(404)
		}
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	return g
}

func (g *crGateway) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.paths)
}

func (g *crGateway) stats() (submits, cancels int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.cancels {
		cancels += n
	}
	return g.submits, cancels
}

func (g *crGateway) lookupCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lookups
}

func cancelOK(string) int             { return http.StatusOK }
func cancelRefused(string) int        { return http.StatusConflict }
func cancelDown(string) int           { return http.StatusServiceUnavailable }
func vendorSentView(id string) []byte { return runningCall(id, true) }

var crCols = []string{"id", "canvas_id", "node_id", "user_id", "run_id", "kind", "backend", "status", "billing_status", "requested_count",
	"backend_ref", "task_id", "request", "created_at", "billing_request_id", "points", "attempt", "cancel_request"}

// crRow 一行 gateway run（已冻结 40 点）。
func crRow(id int64, status int8, attempt int32, cancelReq int8, ref any) *sqlmock.Rows {
	return sqlmock.NewRows(crCols).AddRow(id, "cv_1", "m", 7, "r1", "image", "gateway", status, model.BillHeld, 1,
		ref, "cv_t", `{"model":"Banana","prompt":"p","count":1}`, time.Now(), "canvas-cv_t", 40, attempt, cancelReq)
}

func expectRunGet(mock sqlmock.Sqlmock, id int64, rows *sqlmock.Rows) {
	mock.ExpectQuery(fmt.Sprintf("FROM `afv_canvas_node_run` AS `r` WHERE \\(r.id = %d\\)", id)).WillReturnRows(rows)
}

func crCore(t *testing.T) (*core, sqlmock.Sqlmock) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	return c, mock
}

// 1. 排队中（还没被派发器抢到）用户取消：只打标记，不改状态、不退款，回「取消中」；派发器随后抢 run 的 CAS 因标记落空，
// 由派发器结束并退款——全程零网关调用。
func TestQueuedCancelDispatcherRefundsWithZeroGatewayCalls(t *testing.T) {
	c, mock := crCore(t)
	g := newCRGateway(t, c, waitingCall, cancelOK)
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 0, model.CancelNone, nil))
	expectRequestCancel(mock, 41, true)
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 0)) // 派发器正拿着租约
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 0, model.CancelRequested, nil))
	out, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
	if err != nil || out.Status != "queued" || out.CancelState != "requested" || out.Credits != 40 {
		t.Fatalf("排队中取消应只登记（取消中、不退款）: %+v %v", out, err)
	}
	// 派发器：抢 run 落空 → 重读见已请求取消 → 没提交过（attempt=0）→ 直接取消并退款
	run := gatewayRun(41, model.RunQueued)
	run.BillingStatus, run.Points = model.BillHeld, 40
	run.Request = json.RawMessage(`{"model":"Banana","prompt":"p","count":1}`)
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	expectClaim(mock, 41, 0, false)
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 0, model.CancelRequested, nil))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 4, .*`billing_status` = 'refunded', `error_code` = 'CANCELED'.*`points` = 0 WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), run, sp, ""); err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunCanceled || run.BillingStatus != model.BillRefunded {
		t.Fatalf("派发器应取消并退款: status=%d bill=%s", run.Status, run.BillingStatus)
	}
	if n := g.total(); n != 0 {
		t.Fatalf("没发过请求的 run 取消时不应有任何网关调用: %v", g.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 1b. 排队中、此刻没人拿着租约：CancelRun 就地收尾（本次请求即持有方），同步回 cancelled。
func TestQueuedCancelInlineWhenNobodyHoldsLease(t *testing.T) {
	c, mock := crCore(t)
	g := newCRGateway(t, c, waitingCall, cancelOK)
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 0, model.CancelNone, nil))
	expectRequestCancel(mock, 41, true)
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 0, model.CancelRequested, nil))
	mock.ExpectExec("SET `status` = 4, .*`billing_status` = 'refunded'.* WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunGet(mock, 41, crRow(41, model.RunCanceled, 0, model.CancelRequested, nil))
	out, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
	if err != nil || out.Status != "cancelled" || out.CancelState != "" {
		t.Fatalf("应就地取消: %+v %v", out, err)
	}
	if g.total() != 0 {
		t.Fatalf("零网关调用: %v", g.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 2. 派发器先抢到并已转运行中（用户读到的还是排队）：标记打不上（状态已变），按新状态走运行中取消——由网关裁决是否已发出。
func TestDispatcherWinsThenUserCancelGoesRunningPath(t *testing.T) {
	for name, tc := range map[string]struct {
		view     func(string) []byte
		refunded bool
	}{
		"unsent_cancelled": {waitingCall, true},
		"vendor_sent":      {vendorSentView, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, mock := crCore(t)
			g := newCRGateway(t, c, tc.view, cancelOK)
			expectRunGet(mock, 41, crRow(41, model.RunQueued, 0, model.CancelNone, nil)) // 读到时还在排队
			expectRequestCancel(mock, 41, false)                                         // 派发器已抢到：标记打不上
			expectRunGet(mock, 41, crRow(41, model.RunRunning, 1, model.CancelNone, callA))
			expectRunGet(mock, 41, crRow(41, model.RunRunning, 1, model.CancelNone, callA))
			if tc.refunded {
				mock.ExpectExec("SET `status` = 4, .*`billing_status` = 'refunded'.* WHERE \\(`id` = 41\\) AND \\(`status` = 1\\)").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			out, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
			if tc.refunded {
				if err != nil || out.Status != "cancelled" {
					t.Fatalf("未发出的应取消退款: %+v %v", out, err)
				}
				if _, n := g.stats(); n != 1 {
					t.Fatalf("应取消上游调用一次: %v", g.paths)
				}
			} else {
				if !isVendorSentRefusal(err) {
					t.Fatalf("已发出的应 4002 + CANCEL_VENDOR_SENT: %v", err)
				}
				if _, n := g.stats(); n != 0 {
					t.Fatalf("预检已发出就一条都不取消: %v", g.paths)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 派发器抢 run 时标记还没落：抢到（提交中），之后的取消标记打在提交中的 run 上（不改状态），由派发器提交后收尾。
func TestCancelDuringSubmittingOnlyMarks(t *testing.T) {
	c, mock := crCore(t)
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelNone, nil))
	expectRequestCancel(mock, 41, true)
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	out, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
	if err != nil || out.Status != "submitting" || out.CancelState != "requested" {
		t.Fatalf("提交中取消应只登记: %+v %v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有改状态 / 退款的 UPDATE
		t.Fatal(err)
	}
}

// 3. 提交中被请求取消、提交返回时调用已发给供应商：转运行中的 CAS 因标记落空 → 派发器预检发现已发出 → 不退款、不取消，
// run 转运行中照常跟踪（backend_ref=调用、cancel_request=2，接口 cancel_state=refused），之后按实际结算。
func TestSubmittingCancelVendorSentKeepsTracking(t *testing.T) {
	c, mock := crCore(t)
	g := newCRGateway(t, c, vendorSentView, cancelOK)
	var handed []int64
	c.s.WatchRun = func(id int64) { handed = append(handed, id) }
	run := gatewayRun(41, model.RunQueued)
	run.BillingStatus, run.Points = model.BillHeld, 40
	run.Request = json.RawMessage(`{"model":"Banana","prompt":"p","count":1}`)
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectClaim(mock, 41, 0, true)
	mock.ExpectExec("SET status = 1, .*WHERE \\(id = 41\\) AND \\(status = 5\\) AND \\(cancel_request = 0\\)").WillReturnResult(sqlmock.NewResult(0, 0))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil)) // 提交期间用户请求了取消
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil)) // 收尾前确认仍归本方
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 1, .*`backend_ref` = '" + callA + "', `cancel_request` = 2, .*`progress` = 5.* WHERE \\(`id` = 41\\) AND \\(`status` = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatchInline(context.Background(), run, sp, ""); err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunRunning || run.BillingStatus != model.BillHeld || run.CancelRequest != model.CancelRefused || run.BackendRef.String != callA {
		t.Fatalf("已发出应转运行中继续跟踪、不退款: %+v", run)
	}
	if submits, cancels := g.stats(); submits != 1 || cancels != 0 {
		t.Fatalf("提交一次、预检已发出就不取消: submits=%d cancels=%d", submits, cancels)
	}
	if len(handed) != 1 {
		t.Fatalf("转运行中后应交给长轮询: %v", handed)
	}
	if res := c.toResult(context.Background(), run); res.CancelState != "refused" || res.Status != "running" {
		t.Fatalf("接口应带 cancel_state=refused: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// 之后照常成功：按实际结算（不是取消 / 退款）
	o := ClassifyGateway(model.KindImage, []*aigateway.Call{{ID: callA, Status: aigateway.StatusSucceeded,
		Result: &aigateway.Result{Outputs: []aigateway.OutputItem{{Status: aigateway.StatusSucceeded, Media: &aigateway.MediaOut{URL: "https://c/1.png"}}}}}})
	if !o.done || !o.ok {
		t.Fatalf("应按成功交付结算: %+v", o)
	}
}

// 3b. 同上但调用还没发出：派发器取消调用成功后才取消退款。
func TestSubmittingCancelUnsentRefundsAfterGatewayCancel(t *testing.T) {
	c, mock := crCore(t)
	g := newCRGateway(t, c, waitingCall, cancelOK)
	run := gatewayRun(41, model.RunQueued)
	run.BillingStatus, run.Points = model.BillHeld, 40
	run.Request = json.RawMessage(`{"model":"Banana","prompt":"p","count":1}`)
	sp, _ := c.specFromRun(context.Background(), run)
	expectClaim(mock, 41, 0, true)
	mock.ExpectExec("SET status = 1, .*WHERE \\(id = 41\\) AND \\(status = 5\\) AND \\(cancel_request = 0\\)").WillReturnResult(sqlmock.NewResult(0, 0))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	mock.ExpectExec("SET `status` = 4, .*`billing_status` = 'refunded', `error_code` = 'CANCELED'.* WHERE \\(`id` = 41\\) AND \\(`status` = 5\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), run, sp, ""); err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunCanceled || run.BillingStatus != model.BillRefunded {
		t.Fatalf("未发出应取消退款: %+v", run)
	}
	if _, cancels := g.stats(); cancels != 1 {
		t.Fatalf("应先取消调用: %v", g.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 4. 派发器宕机：带取消标记的 run 停在提交中、租约已失效。兜底退回排队后按幂等键取回调用、网关确认取消成功才退款。
func TestStaleDispatchingCancelRefundsAfterGatewayConfirms(t *testing.T) {
	c, mock := crCore(t)
	c.s.Config.Poll.BatchSize = 100
	g := newCRGateway(t, c, waitingCall, cancelOK)
	mock.ExpectQuery("WHERE \\(r.status = 5\\) AND \\(r.created_at < .*\\) AND \\(r.attempt = 0\\)").WillReturnRows(sqlmock.NewRows(crCols))
	mock.ExpectQuery("WHERE \\(r.status = 5\\) AND \\(r.attempt > 0\\) AND \\(r.updated_at < .*\\) AND \\(\\(r.lease_until IS NULL OR r.lease_until < NOW\\(3\\)\\)\\)").
		WillReturnRows(crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectStaleBackToQueued(mock, 41, 1)
	mock.ExpectExec("SET `status` = 4, .*`billing_status` = 'refunded', `error_code` = 'CANCELED'.* WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	c.advanceSubmitting(context.Background())
	if submits, cancels := g.stats(); submits != 0 || cancels != 1 || g.lookupCount() != 1 {
		t.Fatalf("应按幂等键只查不建地取回一次并取消（不调 Submit）: submits=%d lookups=%d cancels=%d paths=%v", submits, g.lookupCount(), cancels, g.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 4b. 同上但网关取消失败（不可用）：不知道是否已取消 → 不退款，run 留在排队（带标记），下一轮再确认。
func TestStaleDispatchingCancelUnconfirmedNoRefund(t *testing.T) {
	c, mock := crCore(t)
	c.s.Config.Poll.BatchSize = 100
	g := newCRGateway(t, c, waitingCall, cancelDown)
	mock.ExpectQuery("WHERE \\(r.status = 5\\) AND \\(r.created_at < .*\\) AND \\(r.attempt = 0\\)").WillReturnRows(sqlmock.NewRows(crCols))
	mock.ExpectQuery("WHERE \\(r.status = 5\\) AND \\(r.attempt > 0\\)").WillReturnRows(crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectStaleBackToQueued(mock, 41, 1)
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	c.advanceSubmitting(context.Background())
	if _, cancels := g.stats(); cancels != 1 {
		t.Fatalf("应尝试取消: %v", g.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有退款 UPDATE
		t.Fatal(err)
	}
}

// 4c. 排队中、此前提交过（attempt>0）的 run 被用户取消：按幂等键取回的调用已发给供应商 → 不退款，转运行中按实际结算，
// CancelRun 回 4002 + CANCEL_VENDOR_SENT（与运行中取消同一提示），不再只记 orphan ERROR。
func TestQueuedCancelReclaimedVendorSentTracksInsteadOfOrphan(t *testing.T) {
	c, mock := crCore(t)
	g := newCRGateway(t, c, vendorSentView, cancelRefused)
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 2, model.CancelNone, nil))
	expectRequestCancel(mock, 41, true)
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunGet(mock, 41, crRow(41, model.RunQueued, 2, model.CancelRequested, nil))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 1, .*`backend_ref` = '" + callA + "', `cancel_request` = 2.* WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunGet(mock, 41, crRow(41, model.RunRunning, 2, model.CancelRefused, callA))
	_, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
	if !isVendorSentRefusal(err) {
		t.Fatalf("已发出应回 CANCEL_VENDOR_SENT: %v", err)
	}
	if submits, cancels := g.stats(); submits != 0 || cancels != 0 || g.lookupCount() != 1 {
		t.Fatalf("只查不建地取回一次、预检已发出不取消: submits=%d lookups=%d cancels=%d", submits, g.lookupCount(), cancels)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 5. 重复取消幂等：已登记的再取消不再写库、回同一「取消中」；已取消的再取消回当前结果（不报错）。
func TestRepeatCancelIdempotent(t *testing.T) {
	c, mock := crCore(t)
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	expectRequestCancel(mock, 41, false)
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	out, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
	if err != nil || out.CancelState != "requested" || out.Status != "submitting" {
		t.Fatalf("重复取消应幂等返回取消中: %+v %v", out, err)
	}
	expectRunGet(mock, 41, crRow(41, model.RunCanceled, 1, model.CancelRequested, nil))
	out, err = c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 41})
	if err != nil || out.Status != "cancelled" {
		t.Fatalf("已取消再取消应回当前结果: %+v %v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 找回只查不建：网关上没有该幂等键的调用（404）→ 视为没建过单，直接取消退款，且全程不调 Submit（不会自己制造供应商调用）。
func TestReclaimLookup404MeansAbsentNoSubmit(t *testing.T) {
	c, mock := crCore(t)
	g := newCRGateway(t, c, waitingCall, cancelOK)
	g.lookup = http.StatusNotFound
	run := gatewayRun(41, model.RunQueued)
	run.BillingStatus, run.Points, run.Attempt, run.CancelRequest = model.BillHeld, 40, 2, model.CancelRequested
	mock.ExpectExec("SET `status` = 4, .*`billing_status` = 'refunded'.* WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	c.closeQueuedCancel(context.Background(), run, "")
	if submits, cancels := g.stats(); submits != 0 || cancels != 0 || g.lookupCount() != 1 {
		t.Fatalf("404 应视为不存在、不调 Submit / Cancel: submits=%d lookups=%d cancels=%d", submits, g.lookupCount(), cancels)
	}
	if run.Status != model.RunCanceled || run.BillingStatus != model.BillRefunded {
		t.Fatalf("确认没建单后应取消退款: %+v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 找回查询出错（5xx / 冲突）：暂时无法确认 → 不退款、不改状态（没有任何 UPDATE），也不调 Submit。
func TestReclaimLookupErrorNoRefund(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusConflict} {
		c, mock := crCore(t)
		g := newCRGateway(t, c, waitingCall, cancelOK)
		g.lookup = code
		run := gatewayRun(41, model.RunQueued)
		run.BillingStatus, run.Points, run.Attempt, run.CancelRequest = model.BillHeld, 40, 2, model.CancelRequested
		c.closeQueuedCancel(context.Background(), run, "")
		if submits, cancels := g.stats(); submits != 0 || cancels != 0 {
			t.Fatalf("%d: 查询出错不应 Submit / Cancel: submits=%d cancels=%d", code, submits, cancels)
		}
		if run.Status != model.RunQueued || run.BillingStatus != model.BillHeld {
			t.Fatalf("%d: 查询出错不应结束 / 退款: %+v", code, run)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}

// 提交以永久错误结束（系统路径，end.user=false）、但有调用已发出，且提交期间用户请求过取消：
// 转运行中时取消标记一并改成「被拒」（cancel_request=2），前端不会一直停在「取消中」。
func TestSubmitFailVendorSentMarksCancelRefused(t *testing.T) {
	c, mock := crCore(t)
	c.s.Config.Poll.MaxSubmitAttempts = 5
	var mu sync.Mutex
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/calls:batchGet":
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, vendorSentView(callA))
		case r.URL.Path == "/v1/calls" && r.Method == http.MethodPost:
			key := r.Header.Get("Idempotency-Key")
			if strings.HasSuffix(key, ":1") { // 第二张被拒（永久错误）
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"code":"input_rejected","message":"bad prompt"}}`))
				return
			}
			_, _ = w.Write(callJSON(callA, aigateway.StatusPending, ""))
		default:
			w.WriteHeader(404)
		}
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	run := gatewayRun(41, model.RunQueued)
	run.BillingStatus, run.Points, run.RequestedCount = model.BillHeld, 80, 2
	run.Request = json.RawMessage(`{"model":"Banana","prompt":"p","count":2}`)
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	expectClaim(mock, 41, 0, true)
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil)) // 收尾前确认归属：带着用户的取消标记
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 1, .*`backend_ref` = '" + callA + "', `cancel_request` = IF\\(cancel_request = 1, 2, cancel_request\\).* WHERE \\(`id` = 41\\) AND \\(`status` = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), run, sp, ""); err == nil {
		t.Fatal("永久错误应回错误")
	}
	if run.Status != model.RunRunning || run.CancelRequest != model.CancelRefused || run.BillingStatus != model.BillHeld {
		t.Fatalf("有调用已发出应转运行中、标记改被拒、不退款: %+v", run)
	}
	if res := c.toResult(context.Background(), run); res.CancelState != "refused" {
		t.Fatalf("接口应显示 refused: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 抢到 run 之后的写入不随调用方 ctx 取消（RunNode 客户端断开 / RPC 超时）：提交照常完成并转运行中，不会卡在提交中。
func TestDispatchSurvivesCanceledCtx(t *testing.T) {
	c, mock := crCore(t)
	newCRGateway(t, c, waitingCall, cancelOK)
	run := gatewayRun(41, model.RunQueued)
	run.Request = json.RawMessage(`{"model":"Banana","prompt":"p","count":1}`)
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expectClaim(mock, 41, 0, true)
	mock.ExpectExec("SET status = 1, .*`backend_ref` = '" + callA + "'.* WHERE \\(id = 41\\) AND \\(status = 5\\) AND \\(cancel_request = 0\\) AND \\(attempt = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(ctx, run, sp, ""); err != nil || run.Status != model.RunRunning {
		t.Fatalf("ctx 已取消仍应提交并转运行中: status=%d err=%v", run.Status, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// releaseClaim（暂时性错误退回排队）在 ctx 已取消时照样写得进去（带本方 attempt 的 CAS）。
func TestReleaseClaimSurvivesCanceledCtx(t *testing.T) {
	c, mock := crCore(t)
	run := gatewayRun(41, model.RunSubmitting)
	run.Attempt = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock.ExpectExec(regexp.QuoteMeta("SET status = 0, updated_at = NOW(3) WHERE (id = 41) AND (status = 5) AND (cancel_request = 0) AND (attempt = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.releaseClaim(ctx, run, "")
	if run.Status != model.RunQueued {
		t.Fatalf("应退回排队: %d", run.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 提交中兜底：带「待确认的提交失败」的 run 过了 submittingTTL 才按幂等键查——404 才算没建单 → 按记下的失败码失败退款（不退回排队重提）。
func TestStaleUnconfirmedSubmitFailRefundsAfterTTL(t *testing.T) {
	c, mock := crCore(t)
	c.s.Config.Poll.BatchSize = 100
	g := newCRGateway(t, c, waitingCall, cancelOK)
	g.lookup = http.StatusNotFound
	cols := append(append([]string(nil), crCols...), "error_code", "error_reason")
	row := sqlmock.NewRows(cols).AddRow(41, "cv_1", "m", 7, "r1", "image", "gateway", model.RunSubmitting, model.BillHeld, 1,
		nil, "cv_t", `{"model":"Banana","prompt":"p","count":1}`, time.Now(), "canvas-cv_t", 40, 5, 0, "SUBMIT_UNCONFIRMED:SUBMIT_FAILED", "提交失败，请稍后重试")
	mock.ExpectQuery("WHERE \\(r.status = 5\\) AND \\(r.created_at < .*\\) AND \\(r.attempt = 0\\)").WillReturnRows(sqlmock.NewRows(crCols))
	mock.ExpectQuery("WHERE \\(r.status = 5\\) AND \\(r.attempt > 0\\)").WillReturnRows(row)
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 5, model.CancelNone, nil)) // 确认仍无人接手
	mock.ExpectExec("SET `status` = 3, .*`billing_status` = 'refunded', `error_code` = 'SUBMIT_FAILED', `error_reason` = '提交失败，请稍后重试'.* WHERE \\(`id` = 41\\) AND \\(`status` = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	c.advanceSubmitting(context.Background())
	if submits, cancels := g.stats(); submits != 0 || cancels != 0 || g.lookupCount() != 1 {
		t.Fatalf("只查不建、404 即没建单: submits=%d lookups=%d cancels=%d", submits, g.lookupCount(), cancels)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// releaseClaim 取消分支：重读发现 attempt 已变（兜底退回排队后被别的实例重新抢到）→ 不拉回排队、不收尾、不退款。
func TestReleaseClaimCancelBranchAttemptChangedNoop(t *testing.T) {
	c, mock := crCore(t)
	run := gatewayRun(41, model.RunSubmitting)
	run.Attempt = 1
	mock.ExpectExec(regexp.QuoteMeta("SET status = 0, updated_at = NOW(3) WHERE (id = 41) AND (status = 5) AND (cancel_request = 0) AND (attempt = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 2, model.CancelRequested, nil)) // 新持有方（attempt=2）
	c.releaseClaim(context.Background(), run, "")
	if run.Status != model.RunSubmitting {
		t.Fatalf("不应改动新持有方的 run: %d", run.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有别的 UPDATE
		t.Fatal(err)
	}
}

// releaseClaim 取消分支：attempt 一致 → 带 attempt 的 CAS 退回排队，再就地按取消收尾（没提交过的直接取消退款）。
func TestReleaseClaimCancelBranchUsesAttemptCAS(t *testing.T) {
	c, mock := crCore(t)
	newCRGateway(t, c, waitingCall, cancelOK).lookup = http.StatusNotFound
	run := gatewayRun(41, model.RunSubmitting)
	run.Attempt = 1
	mock.ExpectExec(regexp.QuoteMeta("SET status = 0, updated_at = NOW(3) WHERE (id = 41) AND (status = 5) AND (cancel_request = 0) AND (attempt = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectRunGet(mock, 41, crRow(41, model.RunSubmitting, 1, model.CancelRequested, nil))
	expectStaleBackToQueued(mock, 41, 1)
	mock.ExpectExec("SET `status` = 4, .*`billing_status` = 'refunded'.* WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	c.releaseClaim(context.Background(), run, "")
	if run.Status != model.RunCanceled {
		t.Fatalf("应取消: %d", run.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
