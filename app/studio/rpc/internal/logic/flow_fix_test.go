package logic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// qClaim 后台重做占轮次的 CAS（attempt 仍是 n 才 +1）。
func qClaim(n int) string {
	return regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_studio_task` SET attempt = attempt + 1 WHERE (id = 1) AND (attempt = %d) AND (status IN (3, 4)) AND (admin_locked = 0)", n))
}

// expectTaskReq 重读子任务 1，库里的 billing_request_id 为 req。
func expectTaskReq(mock sqlmock.Sqlmock, req string) {
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.id = 1) LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "user_id", "status", "billing_request_id"}).AddRow(1, 9, 7, model.TaskQueued, req))
}

func failedTask(attempt int32) *model.Task {
	tk := runningTask()
	tk.Status, tk.Attempt = model.TaskFailed, attempt
	tk.PromptText.String, tk.PromptText.Valid = "p", true
	return tk
}

func retryBatch(status int8) *model.Batch {
	return &model.Batch{ID: 9, UserID: 7, Kind: model.KindImageGen, Status: status, Payload: []byte(`{"resolution":"1k","aspect_ratio":"1:1"}`)}
}

// 并发两次重做读到同一快照（attempt=0）：只有先占到轮次的一次冻结并成功，另一次在冻结前就被拒，谁的单都不退。
func TestAdminRetryConcurrentOnlyOneWins(t *testing.T) {
	bill := onBill()
	bill.status = resvHeld
	f, mock, _ := newTestFlow(t, bill)
	b := retryBatch(model.BatchGenerating)
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 0") + ".*`billing_request_id` = 'studio-simg_1-r1'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 3)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	// 批次读到时就在生图中：重读一次确认没被收敛成终态（仍生图中 → 不动）
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchGenerating))
	mock.ExpectExec("admin_retry").WillReturnResult(ok1)
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok0) // 第二次：轮次已被占
	if err := f.adminRetry(context.Background(), failedTask(0), b); err != nil {
		t.Fatal(err)
	}
	if err := f.adminRetry(context.Background(), failedTask(0), b); codeOf(err) != xerr.Conflict {
		t.Fatalf("第二次应冲突: %v", err)
	}
	if !reflect.DeepEqual(bill.holds, []string{"studio-simg_1-r1"}) || len(bill.fails) != 0 {
		t.Fatalf("holds=%v fails=%v", bill.holds, bill.fails)
	}
	met(t, mock)
}

// 交错：第二次在第一次占完轮次后才读到任务（attempt=1）→ 占到 r2 并冻结，但状态 CAS 输给第一次；
// 重读库里是 r1（第一次在用）→ 只退自己的 r2，不动 r1。
func TestAdminRetryInterleavedRefundsOnlyOwn(t *testing.T) {
	bill := onBill()
	bill.status = resvHeld
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(qClaim(1)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r2'")).WillReturnResult(ok0)
	expectTaskReq(mock, "studio-simg_1-r1")
	if err := f.adminRetry(context.Background(), failedTask(1), retryBatch(model.BatchGenerating)); codeOf(err) != xerr.Conflict {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(bill.holds, []string{"studio-simg_1-r2"}) || !reflect.DeepEqual(bill.fails, []string{"studio-simg_1-r2"}) {
		t.Fatalf("holds=%v fails=%v", bill.holds, bill.fails)
	}
	met(t, mock)

	// 状态 CAS 失败但重读发现库里就是自己的号（写入其实生效了）：不能退
	bill.holds, bill.fails = nil, nil
	mock.ExpectExec(qClaim(2)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r3'")).WillReturnError(errors.New("conn reset"))
	expectTaskReq(mock, "studio-simg_1-r3")
	if err := f.adminRetry(context.Background(), failedTask(2), retryBatch(model.BatchGenerating)); err == nil {
		t.Fatal("应报错")
	}
	if len(bill.fails) != 0 {
		t.Fatalf("在用的单不应退：%v", bill.fails)
	}
	met(t, mock)
}

// 撤销重做不回退 attempt：再重做换下一个号（r2），不会拿回已退回的 r1。
func TestAdminRetryAfterUndoUsesNewNumber(t *testing.T) {
	bill := onBill()
	bill.status = resvHeld
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r1'")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3")).WillReturnError(errors.New("db down"))
	// 撤回：SET 列按字母序，status/updated_at 之后直接是 billing_request_id —— 没有 attempt（不回退）
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3, `updated_at` = NOW(3), `billing_request_id` = 'studio-simg_1'") + ".*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 0)")).WillReturnResult(ok1)
	expectTaskReq(mock, "studio-simg_1")
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	if err := f.adminRetry(context.Background(), failedTask(0), retryBatch(model.BatchFailed)); err == nil {
		t.Fatal("拉回失败应报错")
	}
	// 再重做：库里 attempt 已是 1
	mock.ExpectExec(qClaim(1)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r2'")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3")).WillReturnResult(ok1)
	mock.ExpectExec("admin_retry").WillReturnResult(ok1)
	if err := f.adminRetry(context.Background(), failedTask(1), retryBatch(model.BatchFailed)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bill.holds, []string{"studio-simg_1-r1", "studio-simg_1-r2"}) || !reflect.DeepEqual(bill.fails, []string{"studio-simg_1-r1"}) {
		t.Fatalf("holds=%v fails=%v", bill.holds, bill.fails)
	}
	met(t, mock)
}

// Hold 拿回的预留单不是 HELD（已退回 / 过期的旧单）：不当成冻结成功，拒绝重做、不改任务。
func TestAdminRetryRejectsNonHeldReservation(t *testing.T) {
	bill := onBill()
	bill.status = resvRefunded
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	if err := f.adminRetry(context.Background(), failedTask(0), retryBatch(model.BatchGenerating)); codeOf(err) != xerr.Conflict {
		t.Fatalf("err=%v", err)
	}
	met(t, mock) // 没有状态 CAS
}

// 已删除批次的任务不能重做（不碰库、不冻结）。
func TestAdminRetryRejectsDeletedBatch(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	b := retryBatch(model.BatchFailed)
	b.Deleted = 1
	if err := f.adminRetry(context.Background(), failedTask(0), b); codeOf(err) != xerr.Conflict {
		t.Fatalf("err=%v", err)
	}
	if len(bill.holds) != 0 {
		t.Fatal("不应冻结")
	}
	met(t, mock)
}

func deliveringTask(since time.Duration) *model.Task {
	tk := runningTask()
	tk.Status = model.TaskDelivering
	tk.ResultRid.String, tk.ResultRid.Valid = "res_1", true
	tk.ResultURL.String, tk.ResultURL.Valid = "https://cdn/x.png", true
	tk.ResultWidth, tk.ResultHeight = 1024, 1024
	tk.UpdatedAt = time.Now().Add(-since)
	return tk
}

// 交付中 + result_url 已写，gateway 不可用：不依赖 Get，直接结算并置成功（成本记 0，镜像已结算）。
func TestResumeDeliverWithoutGateway(t *testing.T) {
	bill := onBill()
	f, mock, gw := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`billing_status` = 2, `cost_cny` = '0'.*`result_url` = 'https://cdn/x.png'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 5)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.resumeDeliver(context.Background(), deliveringTask(time.Minute), batch9, time.Now())
	if !reflect.DeepEqual(bill.settles, []string{"studio-simg_1"}) || gw.gets != 1 {
		t.Fatalf("settles=%v gets=%d", bill.settles, gw.gets)
	}
	met(t, mock)
}

// 交付中未超期、结算暂时失败：留在交付中，不改库。
func TestResumeDeliverTransientSettleStays(t *testing.T) {
	bill := onBill()
	bill.settleErr = errors.New("db blip")
	f, mock, _ := newTestFlow(t, bill)
	f.resumeDeliver(context.Background(), deliveringTask(time.Minute), batch9, time.Now())
	if len(bill.fails) != 0 {
		t.Fatalf("fails=%v", bill.fails)
	}
	met(t, mock)
}

// 交付中超期、结算持续失败、预留单仍 HELD：置失败（交付超时）并退款。
func TestResumeDeliverOverdueHeldRefunds(t *testing.T) {
	bill := onBill()
	bill.settleErr = errors.New("db blip")
	bill.status = resvHeld
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`billing_status` = 3.*`error_reason` = '交付超时，点数已退回'.*`result_url` = NULL.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 5)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.resumeDeliver(context.Background(), deliveringTask(31*time.Minute), batch9, time.Now())
	if !reflect.DeepEqual(bill.fails, []string{"studio-simg_1"}) {
		t.Fatalf("fails=%v", bill.fails)
	}
	met(t, mock)
}

// 交付中超期、Settle ErrGone 且预留单已释放：置失败「交付超时，点数已退回」，不再退款。
func TestResumeDeliverOverdueReleased(t *testing.T) {
	bill := onBill()
	bill.settleErr = billing.ErrGone
	bill.status = resvExpired
	f, mock, _ := newTestFlow(t, bill)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 3") + ".*`error_reason` = '交付超时，点数已退回'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 5)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.resumeDeliver(context.Background(), deliveringTask(31*time.Minute), batch9, time.Now())
	if len(bill.fails) != 0 {
		t.Fatalf("已释放的不应再退：%v", bill.fails)
	}
	met(t, mock)
}

// 双实例同时交付：本实例 Settle 返回 ErrGone，但预留单已 CONFIRMED（对方已结算）→ 照常交付成功，不置失败、不退款。
func TestDeliverSettleGoneButConfirmed(t *testing.T) {
	for _, st := range []string{resvConfirmed, resvPartiallyRefunded} {
		t.Run(st, func(t *testing.T) {
			bill := onBill()
			bill.settleErr = fmt.Errorf("%w: 状态已变化", billing.ErrGone)
			bill.status = st
			f, mock, _ := newTestFlow(t, bill)
			mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5")).WillReturnResult(ok1)
			mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`billing_status` = 2.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 5)")).WillReturnResult(ok1)
			mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
			mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
			expectConvergeFinal(mock)
			f.deliver(context.Background(), runningTask(), batch9, succeededCall(), time.Now())
			if len(bill.fails) != 0 || len(bill.confirmed) != 0 {
				t.Fatalf("fails=%v confirmed=%v", bill.fails, bill.confirmed)
			}
			met(t, mock)
		})
	}
}

// Mode=on 时结算成功即写镜像 2，哪怕镜像停在 0（写失败过）。
func TestDeliverWritesSettledMirrorEvenIfNotHeld(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	tk := runningTask()
	tk.BillingStatus = model.BillingNone
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`billing_status` = 2.*").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), tk, batch9, succeededCall(), time.Now())
	met(t, mock)
}

// 超时取消返回 SUCCEEDED 但不带结果：再 GET 一次拿到成品后照常交付。
func TestCancelOnTimeoutSucceededRefetches(t *testing.T) {
	bill := onBill()
	f, mock, gw := newTestFlow(t, bill)
	gw.cancelStatus = aigateway.StatusSucceeded
	gw.getCall = succeededCall()
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.cancelOnTimeout(context.Background(), runningTask(), batch9, &aigateway.Call{ID: "call_1", Status: aigateway.StatusRunning}, time.Now())
	if gw.gets != 1 || !reflect.DeepEqual(bill.settles, []string{"studio-simg_1"}) {
		t.Fatalf("gets=%d settles=%v", gw.gets, bill.settles)
	}
	met(t, mock)
}

func TestDeliveryOverdue(t *testing.T) {
	now := time.Now()
	if !deliveryOverdue(deliveringTask(31*time.Minute), now, 30*time.Minute) {
		t.Fatal("超过宽限期应算超期")
	}
	if deliveryOverdue(deliveringTask(29*time.Minute), now, 30*time.Minute) {
		t.Fatal("宽限期内不算超期")
	}
	tk := deliveringTask(time.Hour)
	tk.Status = model.TaskRunning
	if deliveryOverdue(tk, now, 30*time.Minute) {
		t.Fatal("只有交付中才算")
	}
}

func expectBatchTasks(mock sqlmock.Sqlmock, statuses ...int8) {
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchGenerating))
	rows := sqlmock.NewRows([]string{"id", "batch_id", "user_id", "status"})
	for i, st := range statuses {
		rows.AddRow(i+1, 9, 7, st)
	}
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 9) ORDER BY")).WillReturnRows(rows)
}

// 派发：交付中占本批名额；全局生成中达到上限不派发；派发时 started_at 置空（真正提交 gateway 时才写）。
func TestSubmitBatchLimits(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	// 2 交付中 + 1 生成中 = 本批 3 个名额已满
	expectBatchTasks(mock, model.TaskDelivering, model.TaskDelivering, model.TaskRunning, model.TaskQueued)
	if got := f.submitBatch(context.Background(), 9); len(got) != 0 {
		t.Fatalf("交付中应占名额：%v", got)
	}
	met(t, mock)

	// 全局已有 64 个生成中
	expectBatchTasks(mock, model.TaskQueued, model.TaskQueued)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM `afv_studio_task` AS `t` WHERE (t.status = 1)")).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(64))
	if got := f.submitBatch(context.Background(), 9); len(got) != 0 {
		t.Fatalf("全局达上限不应派发：%v", got)
	}
	met(t, mock)

	// 全局还剩 1 个名额：只派发 1 个，started_at 置空
	expectBatchTasks(mock, model.TaskQueued, model.TaskQueued)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*)")).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(63))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 1, `updated_at` = NOW(3), `started_at` = NULL WHERE (`id` = 1) AND (`status` = 0)")).WillReturnResult(ok1)
	if got := f.submitBatch(context.Background(), 9); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("got=%v", got)
	}
	met(t, mock)
}
