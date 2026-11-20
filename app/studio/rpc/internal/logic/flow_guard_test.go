package logic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/semaphore"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

// fakeFlowBill flowBiller 的假实现：记录续期 / 结算 / 退款调用；status 是 Status 返回的预留单状态。
type fakeFlowBill struct {
	fakeBill
	renews     []string
	settles    []string
	confirmed  []string
	status     string
	settleErr  error
	enoughs    []billing.Charge // Enough 收到的计费参数
	enoughErr  error            // Enough 的返回（余额预检失败）
	actualCost bool             // 模拟载体按实际用量结算：SettleCost 会调成本来源
	costUSD    []string         // 成本来源取到的美元成本（取不到记 unavailable）
	mu         sync.Mutex       // renewHeld 并发续期
}

func (f *fakeFlowBill) Renew(_ context.Context, _ int64, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews = append(f.renews, requestID)
	return nil
}
func (f *fakeFlowBill) SettleCost(ctx context.Context, _ int64, requestID string, _ int, cost billing.CostFunc) (*billing.Settlement, error) {
	f.settles = append(f.settles, requestID)
	if f.settleErr != nil {
		return nil, f.settleErr
	}
	out := &billing.Settlement{Mode: billing.SettleFixed}
	if f.actualCost && cost != nil {
		out.Mode = billing.SettleActualCost
		if usd, ok := cost(ctx); ok {
			f.costUSD = append(f.costUSD, usd.FloatString(6))
			out.ActualCost = true
		} else {
			f.costUSD = append(f.costUSD, "unavailable")
		}
	}
	return out, nil
}
func (f *fakeFlowBill) Status(context.Context, int64, string) (string, error) { return f.status, nil }
func (f *fakeFlowBill) Enough(_ context.Context, c billing.Charge) error {
	f.enoughs = append(f.enoughs, c)
	return f.enoughErr
}
func (f *fakeFlowBill) RefundConfirmed(_ context.Context, _ int64, requestID, _ string) (int64, error) {
	f.confirmed = append(f.confirmed, requestID)
	return 10, nil
}

// fakeGW 网关：Cancel 返回 cancelStatus 的调用并记录；Get 返回 getCall（为 nil 时报 gateway 不可用）。
type fakeGW struct {
	pipeline.Gateway
	cancels      []string
	cancelStatus string
	cancelErr    error // 非空时 Cancel 返回它（如 409 CANCEL_NOT_ALLOWED）
	getCall      *aigateway.Call
	gets         int
}

func (g *fakeGW) Get(context.Context, string, time.Duration) (*aigateway.Call, error) {
	g.gets++
	if g.getCall == nil {
		return nil, errors.New("gateway unavailable")
	}
	return g.getCall, nil
}

func (g *fakeGW) Cancel(_ context.Context, id string) (*aigateway.Call, error) {
	g.cancels = append(g.cancels, id)
	if g.cancelErr != nil {
		return nil, g.cancelErr
	}
	return &aigateway.Call{ID: id, Status: g.cancelStatus}, nil
}

// fakeMedia 只实现转存。
type fakeMedia struct{ pipeline.Media }

func (fakeMedia) CopyToLibrary(context.Context, string, string, string) (mediasvc.Resource, error) {
	return mediasvc.Resource{RID: "res_1", URL: "https://cdn/x.png", Width: 1024, Height: 1024}, nil
}

func newTestFlow(t *testing.T, bill *fakeFlowBill) (*flow, sqlmock.Sqlmock, *fakeGW) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	gw := &fakeGW{cancelStatus: aigateway.StatusCancelled}
	return &flow{
		repo: repo.New(bun.NewDB(sdb, mysqldialect.New())), bill: bill, gw: gw, gwOn: true, media: fakeMedia{},
		sem: semaphore.NewGroup(), now: time.Now, sync: func(context.Context) {},
	}, mock, gw
}

func onBill() *fakeFlowBill { return &fakeFlowBill{fakeBill: fakeBill{mode: billing.ModeOn}} }

func runningTask() *model.Task {
	tk := &model.Task{ID: 1, BatchID: 9, UserID: 7, Kind: model.KindImageGen, Status: model.TaskRunning, BillingStatus: model.BillingHeld}
	tk.TaskID.String, tk.TaskID.Valid = "simg_1", true
	tk.BillingRequestID.String, tk.BillingRequestID.Valid = "studio-simg_1", true
	tk.GatewayCallID.String, tk.GatewayCallID.Valid = "call_1", true
	return tk
}

func succeededCall() *aigateway.Call {
	return &aigateway.Call{ID: "call_1", Status: aigateway.StatusSucceeded, Result: &aigateway.Result{
		Outputs: []aigateway.OutputItem{{Status: aigateway.StatusSucceeded, Media: &aigateway.MediaOut{Rid: "out_1", Width: 1024, Height: 1024}}},
	}}
}

var (
	batch9 = &model.Batch{ID: 9, UserID: 7, Status: model.BatchGenerating, CreatedAt: time.Now()}
	ok1    = sqlmock.NewResult(0, 1)
	ok0    = sqlmock.NewResult(0, 0)
	qB9    = regexp.QuoteMeta("WHERE (b.id = 9) LIMIT 1")
)

// converge 读到批次已终态即返回。
func expectConvergeFinal(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
}

// tick 一轮：排队中的已冻结子任务（编排阶段）也被续期。
func TestTickRenewsQueuedHeld(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	f.gwOn = false
	mock.ExpectQuery(regexp.QuoteMeta("JOIN afv_studio_batch AS b ON b.id = t.batch_id WHERE (b.status IN (0, 1, 2, 3)) AND (t.status IN (0, 1, 5)) AND (t.billing_request_id IS NOT NULL AND t.billing_request_id <> '') AND (t.id > 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "user_id", "billing_request_id", "billing_status"}).
			AddRow(1, 9, 7, "studio-simg_1", model.BillingHeld).AddRow(2, 9, 7, "studio-simg_2", model.BillingHeld))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = 8) AND (b.created_at < FROM_UNIXTIME(")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT t.batch_id")).WillReturnRows(sqlmock.NewRows([]string{"batch_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("t.billing_status = 1")).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "billing_request_id", "billing_status"}))
	for _, st := range []string{"0", "1", "2", "3"} {
		mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = " + st + ") AND ((b.lease_until IS NULL OR b.lease_until < NOW(3))) ORDER BY")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	}
	f.tick(context.Background())
	f.wg.Wait()
	if len(bill.renews) != 2 {
		t.Fatalf("排队的已冻结子任务应续期：%v", bill.renews)
	}
	met(t, mock)

	// 一分钟内的下一轮不重复续期
	for _, st := range []string{"0", "1", "2", "3"} {
		mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = " + st + ") AND ((b.lease_until IS NULL OR b.lease_until < NOW(3))) ORDER BY")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	}
	f.tick(context.Background())
	if len(bill.renews) != 2 {
		t.Fatalf("maintainEvery 内不应再续期：%v", bill.renews)
	}
	met(t, mock)
}

// 竞态顺序 1：强制失败先抢到 生成中→失败，Poller 随后交付时抢不到交付锁，不结算。
func TestForceFailThenDeliverNoSettle(t *testing.T) {
	bill := onBill()
	bill.status = "REFUNDED"
	f, mock, gw := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`admin_locked` = 1.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 1)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1 WHERE (id = 9)")).WillReturnResult(ok1)
	mock.ExpectExec("admin_force_fail").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	if err := f.forceFail(context.Background(), runningTask(), batch9); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bill.fails, []string{"studio-simg_1"}) || !reflect.DeepEqual(gw.cancels, []string{"call_1"}) {
		t.Fatalf("fails=%v cancels=%v", bill.fails, gw.cancels)
	}

	// Poller 手里的还是旧快照（生成中）：交付锁 CAS 落空
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 5") + ".*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 1)")).WillReturnResult(ok0)
	f.deliver(context.Background(), runningTask(), batch9, succeededCall(), time.Now())
	if len(bill.settles) != 0 {
		t.Fatalf("没抢到交付锁不应结算：%v", bill.settles)
	}
	met(t, mock)
}

// 竞态顺序 2：Poller 先抢到交付锁并结算成功；之后的强制失败（无论读到交付中还是旧的生成中）都不能改失败、不退款。
func TestDeliverThenForceFailRejected(t *testing.T) {
	bill := onBill()
	f, mock, gw := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 5") + ".*`result_url` = 'https://cdn/x.png'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 1)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`billing_status` = 2.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 5)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, succeededCall(), time.Now())
	if !reflect.DeepEqual(bill.settles, []string{"studio-simg_1"}) {
		t.Fatalf("settles=%v", bill.settles)
	}
	met(t, mock)

	// 读到交付中：直接拒绝，不碰库、不取消、不退款
	tk := runningTask()
	tk.Status = model.TaskDelivering
	if err := f.forceFail(context.Background(), tk, batch9); err == nil {
		t.Fatal("交付中应拒绝强制失败")
	} else if _, msg, _ := xerr.Parse(err); msg != reasonDelivering {
		t.Fatalf("err=%v", err)
	}
	// 读到旧的生成中：CAS 落空，报状态已变化
	mock.ExpectExec(regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 1)")).WillReturnResult(ok0)
	if err := f.forceFail(context.Background(), runningTask(), batch9); codeOf(err) != xerr.Conflict {
		t.Fatalf("err=%v", err)
	}
	if len(bill.fails) != 0 || len(bill.confirmed) != 0 {
		t.Fatalf("已结算的不应退款：fails=%v confirmed=%v", bill.fails, bill.confirmed)
	}
	_ = gw
	met(t, mock)
}

// 强制失败前取消 gateway 调用，返回已成功（出图了）：拒绝，交给 Poller 照常交付。
func TestForceFailCancelReturnsSucceeded(t *testing.T) {
	bill := onBill()
	f, mock, gw := newTestFlow(t, bill)
	gw.cancelStatus = aigateway.StatusSucceeded
	if err := f.forceFail(context.Background(), runningTask(), batch9); codeOf(err) != xerr.Conflict {
		t.Fatalf("err=%v", err)
	}
	if len(bill.fails) != 0 {
		t.Fatalf("fails=%v", bill.fails)
	}
	met(t, mock)
}

// cancelRefused 网关的取消被拒（已发给供应商）。
var cancelRefused = &aigateway.Error{HTTPStatus: 409, Code: aigateway.CodeCancelNotAllowed, Check: aigateway.CheckCancelVendorSent, CallID: "call_1"}

// 强制失败时网关拒绝取消（已发给供应商）：运营决定照旧生效（失败退款），只打日志、不崩、不重试取消。
func TestForceFailCancelRefusedStillFails(t *testing.T) {
	bill := onBill()
	bill.status = "REFUNDED"
	f, mock, gw := newTestFlow(t, bill)
	gw.cancelErr = cancelRefused
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`admin_locked` = 1.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 1)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1 WHERE (id = 9)")).WillReturnResult(ok1)
	mock.ExpectExec("admin_force_fail").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	if err := f.forceFail(context.Background(), runningTask(), batch9); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gw.cancels, []string{"call_1"}) || !reflect.DeepEqual(bill.fails, []string{"studio-simg_1"}) {
		t.Fatalf("cancels=%v fails=%v", gw.cancels, bill.fails)
	}
	met(t, mock)
}

// 失败退款时预留单已 CONFIRMED（已扣费）：走已确认退款。
func TestFailTaskRefundsConfirmed(t *testing.T) {
	bill := onBill()
	bill.status = resvConfirmed
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`billing_status` = 3.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 1)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.failTask(context.Background(), runningTask(), batch9, reasonTimeout, true, "")
	if !reflect.DeepEqual(bill.fails, []string{"studio-simg_1"}) || !reflect.DeepEqual(bill.confirmed, []string{"studio-simg_1"}) {
		t.Fatalf("fails=%v confirmed=%v", bill.fails, bill.confirmed)
	}
	met(t, mock)
}

// 预留已失效（Settle ErrGone）：从交付中置失败、清成品地址、不退款。
func TestDeliverSettleGone(t *testing.T) {
	bill := onBill()
	bill.settleErr = billing.ErrGone
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`result_url` = NULL.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 5)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, succeededCall(), time.Now())
	if len(bill.fails) != 0 {
		t.Fatalf("ErrGone 不应退款：%v", bill.fails)
	}
	met(t, mock)
}

// 后台重做：批次拉回出错 → 子任务 CAS 回原状态、新预留单退回、报错。
func TestAdminRetryBatchRevertFails(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	tk := runningTask()
	tk.Status, tk.Attempt = model.TaskFailed, 0
	tk.PromptText.String, tk.PromptText.Valid = "p", true
	tk.ErrorReason.String, tk.ErrorReason.Valid = "生成失败，点数已退回", true
	bill.status = resvHeld
	b := &model.Batch{ID: 9, UserID: 7, Kind: model.KindImageGen, Status: model.BatchFailed, Payload: []byte(`{"resolution":"1k","aspect_ratio":"1:1"}`)}
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 0") + ".*`billing_request_id` = 'studio-simg_1-r1'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 3)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("WHERE (`id` = 9) AND (`status` = 5)")).WillReturnError(errors.New("db down"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`billing_request_id` = 'studio-simg_1'.*`error_reason` = '生成失败，点数已退回'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 0)")).WillReturnResult(ok1)
	expectTaskReq(mock, "studio-simg_1") // 撤回后重读：库里已是旧号 → 新单没被用上，退回
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	if err := f.adminRetry(context.Background(), tk, b); err == nil {
		t.Fatal("拉回失败应报错")
	}
	if !reflect.DeepEqual(bill.holds, []string{"studio-simg_1-r1"}) || !reflect.DeepEqual(bill.fails, []string{"studio-simg_1-r1"}) {
		t.Fatalf("holds=%v fails=%v", bill.holds, bill.fails)
	}
	met(t, mock)
}

// 提交中超过 5 分钟（提交进程崩溃）：批次 提交中→失败，有计费请求号的排队子任务逐个置失败并退款。
func TestReapSubmitting(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = 8) AND (b.created_at < FROM_UNIXTIME(")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "task_total"}).AddRow(9, 7, model.BatchSubmitting, 2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 5") + ".*`task_failed` = 2 " + regexp.QuoteMeta("WHERE (`id` = 9) AND (`status` = 8)")).WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 9) ORDER BY")).WillReturnRows(sqlmock.NewRows(taskCols).
		AddRow(1, 9, 7, model.TaskQueued, model.BillingHeld, "studio-a").
		AddRow(2, 9, 7, model.TaskQueued, model.BillingNone, "studio-b"))
	mock.ExpectExec(regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 0)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("WHERE (`id` = 2) AND (`status` = 0)")).WillReturnResult(ok1)
	mock.ExpectExec("submit_aborted").WillReturnResult(ok1)
	f.reapSubmitting(context.Background())
	if !reflect.DeepEqual(bill.fails, []string{"studio-a", "studio-b"}) {
		t.Fatalf("fails=%v", bill.fails)
	}
	met(t, mock)
}

// 终态批次里有冻结有效的排队子任务（后台重做后拉回失败）：批次拉回 生图中。
func TestRescueStrayQueued(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT t.batch_id")).WillReturnRows(sqlmock.NewRows([]string{"batch_id"}).AddRow(9))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchFailed))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 9) ORDER BY")).WillReturnRows(sqlmock.NewRows(append(taskCols, "prompt_text")).
		AddRow(1, 9, 7, model.TaskQueued, model.BillingHeld, "studio-a-r1", "p").
		AddRow(2, 9, 7, model.TaskFailed, model.BillingRefunded, "studio-b", "p"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("WHERE (`id` = 9) AND (`status` = 5)")).WillReturnResult(ok1)
	mock.ExpectExec("revived").WillReturnResult(ok1)
	f.rescueStrayQueued(context.Background())
	met(t, mock)
}

// 终态任务的计费镜像对账：按预留单真实状态回写，仍 HELD 的不动；写入走 CAS（billing_status = 已冻结）。
func TestReconcileBilling(t *testing.T) {
	cases := []struct {
		resv string
		to   int8
		want bool
	}{
		{"EXPIRED", model.BillingRefunded, true},
		{"REFUNDED", model.BillingRefunded, true},
		{"CONFIRMED", model.BillingSettled, true},
		{"PARTIALLY_REFUNDED", model.BillingSettled, true},
		{"HELD", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		t.Run("resv_"+c.resv, func(t *testing.T) {
			bill := onBill()
			bill.status = c.resv
			f, mock, _ := newTestFlow(t, bill)
			mock.ExpectQuery(regexp.QuoteMeta("t.status IN (2, 3, 4)")).
				WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "billing_request_id", "billing_status"}).
					AddRow(5, 7, "studio-simg_5", model.BillingHeld))
			if c.want {
				mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("SET billing_status = %d WHERE (id = 5) AND (billing_status = 1)", c.to))).WillReturnResult(ok1)
			}
			f.reconcileBilling(context.Background())
			met(t, mock) // HELD / 无单：没有预期的 UPDATE，误写会触发 sqlmock 的未预期 Exec
		})
	}
}

// Mode 不是 on 时不对账（不查库）。
func TestReconcileBillingOffMode(t *testing.T) {
	f, mock, _ := newTestFlow(t, &fakeFlowBill{fakeBill: fakeBill{mode: billing.ModeOff}})
	f.reconcileBilling(context.Background())
	met(t, mock)
}

// 清理预览缓存：满批继续、不满批停；间隔内不重复；间隔 <= 0 关闭；保留期被夹到不小于复用窗口。
func TestCleanPreviewCache(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.cfg.Poll.PreviewCacheCleanMinutes = 10
	f.cfg.Poll.PreviewCacheCleanBatch = 2
	f.cfg.Poll.PreviewCacheRetainHours = 1 // 小于复用窗口 2h，应夹到 2h = 7200s
	del := regexp.QuoteMeta("DELETE FROM afv_studio_prompt_cache WHERE (batch_id < 0) AND (created_at < DATE_SUB(NOW(3), INTERVAL 7200 SECOND)) LIMIT 2")
	mock.ExpectExec(del).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(del).WillReturnResult(sqlmock.NewResult(0, 1))
	f.cleanPreviewCache(context.Background())
	f.cleanPreviewCache(context.Background()) // 间隔内：不再发 SQL
	met(t, mock)

	g, gmock, _ := newTestFlow(t, onBill()) // 间隔 0：关闭，不发任何 SQL
	g.cleanPreviewCache(context.Background())
	met(t, gmock)
}
