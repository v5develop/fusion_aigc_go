package logic

// 付费产物入库可靠性（S1 同步 / 免费档入库失败不吞错、S2 永久失败定时重投、对账补入）。

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/asset"

	"github.com/DATA-DOG/go-sqlmock"
)

// S1：付费同步转存入库失败 → 不置成功，留在交付中下一轮续做（已结算一次，不退款）。
func TestDeliverSyncAssetFailureStaysDelivering(t *testing.T) {
	f, mock, bill, _, _ := asyncFlow(t)
	f.cfg.Library.AsyncCopy = false
	f.asset = func(context.Context, []asset.Asset) error { return errors.New("db blip") }
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_url` = '" + libURL + "'").WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
	met(t, mock) // 没有 SET `status` = 2：仍在交付中
	if len(bill.settles) != 1 || len(bill.fails) != 0 {
		t.Fatalf("应已结算一次、不退款: settles=%v fails=%v", bill.settles, bill.fails)
	}
}

// S1：交付中续做超过 deliverGrace 仍入不了库 → 照常置成功（不能拖住已付费成品、更不能置失败），留给对账补入。
func TestResumeDeliverOverdueAssetFailureStillDelivers(t *testing.T) {
	f, mock, bill, _, _ := asyncFlow(t)
	f.gwOn = false
	f.asset = func(context.Context, []asset.Asset) error { return errors.New("db blip") }
	tk := runningTask()
	tk.Status = model.TaskDelivering
	tk.ResultRid.String, tk.ResultRid.Valid = wmRID, true
	tk.ResultURL.String, tk.ResultURL.Valid = libURL, true
	tk.UpdatedAt = time.Now().Add(-24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = '" + libURL + "'").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.resumeDeliver(context.Background(), tk, batch9, time.Now())
	met(t, mock)
	if len(bill.fails) != 0 {
		t.Fatal("入库失败不能退款 / 置失败")
	}
}

// S1：交付中续做（未超时）入库成功 → 置成功；同一成品已入过库（崩溃在入库与置成功之间）不再写第二条。
func TestResumeDeliverAfterCrashBetweenAssetAndSuccess(t *testing.T) {
	f, mock, _, _, saved := asyncFlow(t)
	f.gwOn = false
	tk := runningTask()
	tk.Status = model.TaskDelivering
	tk.ResultRid.String, tk.ResultRid.Valid = wmRID, true
	tk.ResultURL.String, tk.ResultURL.Valid = libURL, true
	tk.UpdatedAt = time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("simg_1", 33))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.resumeDeliver(context.Background(), tk, batch9, time.Now())
	met(t, mock)
	if len(*saved) != 0 {
		t.Fatalf("已入库的成品不应再写: %+v", *saved)
	}
}

// S1：免费档（不扣费）入库失败照常交付，但不吞错（ERROR check=studio.free_asset_failed）。
func TestDeliverFreeTierAssetFailureStillDelivers(t *testing.T) {
	f, mock, _, _, _ := asyncFlow(t)
	f.tier = fakeTier{free: true}
	f.asset = func(context.Context, []asset.Asset) error { return errors.New("db blip") }
	ref := regexp.QuoteMeta("'mrw://" + wmRID + ".png'")
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
	met(t, mock)
}

var studioJobCols = []string{"id", "task_pk", "task_id", "user_id", "src_rid", "ref", "status", "attempt", "next_at", "last_error", "created_at", "updated_at", "finished_at"}

// S2：永久失败行（不含作废）按代退避自动重投，attempt CAS；并发已被重投的跳过；只在到间隔时扫。
func TestStudioRequeueFailedCopies(t *testing.T) {
	f, mock, _, _, _ := asyncFlow(t)
	f.cp = nil // 没有转存池：重投后等扫描接手
	f.cfg.Library.AutoRequeueMax = 2
	f.cfg.Library.AutoRequeueBatch = 5
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_studio_copy_job` AS `j` WHERE (j.status = 2) AND (j.attempt < 2000) AND (finished_at IS NOT NULL AND finished_at <= NOW(3) - INTERVAL (CASE attempt DIV 1000 WHEN 0 THEN 1800 WHEN 1 THEN 3600 ELSE 7200 END) SECOND) ORDER BY j.finished_at ASC, j.id ASC LIMIT 5")).
		WillReturnRows(sqlmock.NewRows(studioJobCols).
			AddRow(5, 1, "simg_1", 7, wmRID, paidRef, model.CopyFailed, 10, now, "x", now, now, now).
			AddRow(6, 2, "simg_2", 7, wmRID, paidRef, model.CopyFailed, 3, now, "x", now, now, now))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_copy_job` SET status = 0, attempt = (attempt DIV 1000 + 1) * 1000, next_at = NOW(3), finished_at = NULL WHERE (id = 5) AND (status = 2) AND (attempt = 10)")).
		WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("WHERE (id = 6) AND (status = 2) AND (attempt = 3)")).WillReturnResult(ok0)
	f.requeueTick(context.Background())
	f.requeueTick(context.Background()) // 不到间隔：不再查库
	met(t, mock)
	if !f.autoRequeueLeft(1005) || f.autoRequeueLeft(2001) {
		t.Fatal("代数判定不符")
	}
}

// 对账：抢到子任务租约 → 重读（成功、已结算、公开地址）→ 入库 → 释放租约；租约被持有时不补（报错，下次再看）。
func TestStudioReconcileTask(t *testing.T) {
	f, mock, _, _, saved := asyncFlow(t)
	f.owner = "me"
	qLease := regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_owner` = ")
	qRelease := regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_owner` = NULL")
	row := func(billing int8, url string) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "batch_id", "user_id", "kind", "task_id", "status", "billing_status", "result_rid", "result_url", "deleted"}).
			AddRow(1, 9, 7, "imagegen", "simg_1", model.TaskSucceeded, billing, wmRID, url, 0)
	}
	mock.ExpectExec(qLease).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(row(model.BillingSettled, libURL))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(qRelease).WillReturnResult(ok1)
	if wrote, err := f.reconcileTask(context.Background(), 1); err != nil || !wrote {
		t.Fatalf("应补入: wrote=%v err=%v", wrote, err)
	}
	if len(*saved) != 1 || (*saved)[0].ResultURL != libURL || (*saved)[0].SourceTaskID != "simg_1" {
		t.Fatalf("补入的行不符: %+v", *saved)
	}
	// 租约被交付持有：不补
	mock.ExpectExec(qLease).WillReturnResult(ok0)
	if _, err := f.reconcileTask(context.Background(), 1); !errors.Is(err, errReconcileBusy) {
		t.Fatalf("租约被持有应报忙: %v", err)
	}
	// 重读时已不是公开地址（如被重做）：不补
	mock.ExpectExec(qLease).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(row(model.BillingSettled, paidRef))
	mock.ExpectExec(qRelease).WillReturnResult(ok1)
	if wrote, err := f.reconcileTask(context.Background(), 1); err != nil || wrote {
		t.Fatalf("私有引用不归对账: wrote=%v err=%v", wrote, err)
	}
	met(t, mock)
}
