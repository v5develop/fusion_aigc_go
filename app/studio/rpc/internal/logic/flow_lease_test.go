package logic

import (
	"context"
	"regexp"
	"testing"

	"aigc-local-life/app/studio/model"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	qBatchLease = regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `lease_owner` = 'inst-a', `lease_until` = DATE_ADD(NOW(3), INTERVAL ")
	qTaskLease  = regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_owner` = 'inst-a', `lease_until` = DATE_ADD(NOW(3), INTERVAL ")
	freeB       = regexp.QuoteMeta("AND ((b.lease_until IS NULL OR b.lease_until < NOW(3))) ORDER BY")
	freeT       = regexp.QuoteMeta("AND ((t.lease_until IS NULL OR t.lease_until < NOW(3))) ORDER BY")
)

// 编排：候选批次抢不到租约（别的实例在编排）→ 跳过，不读批次、不调 sidecar。
func TestAdvanceBatchesSkipsLeasedBatch(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.owner = "inst-a"
	mock.MatchExpectationsInOrder(false) // 抢租约在协程里，与后两个状态的列表查询先后不定
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = 0) ") + freeB).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(9, model.BatchQueued))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = 1) ") + freeB).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = 2) ") + freeB).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(qBatchLease + ".*WHERE `id` = 9 AND").WillReturnResult(ok0)
	f.advanceBatches(context.Background())
	f.wg.Wait()
	met(t, mock) // 之后没有任何 GetByID / CountByBatch：没走 orchestrate
}

// 派发：生成中 的批次抢不到租约 → 不派发子任务。
func TestSubmitGeneratingSkipsLeasedBatch(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.owner = "inst-a"
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.status = 3) ") + freeB).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(9, model.BatchGenerating))
	mock.ExpectExec(qBatchLease + ".*WHERE `id` = 9 AND").WillReturnResult(ok0)
	f.submitGenerating(context.Background())
	met(t, mock)
}

// 轮询：子任务抢不到租约（别的实例在轮询）→ 跳过，不查库、不调 gateway；抢到的才走 pollTask，结束释放。
func TestPollRunningSkipsLeasedTask(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.owner = "inst-a"
	mock.MatchExpectationsInOrder(false) // 抢租约在协程里，与 交付中 的列表查询先后不定
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.status = 1) AND (t.admin_locked = 0) ") + freeT).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(1, model.TaskRunning))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.status = 5) AND (t.admin_locked = 0) ") + freeT).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(qTaskLease + ".*WHERE `id` = 1 AND").WillReturnResult(ok0)
	f.pollRunning(context.Background())
	f.wg.Wait()
	met(t, mock)

	// 抢到：pollTask 读子任务（这里读到已终态直接返回），随后释放租约
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.status = 1) AND (t.admin_locked = 0) ") + freeT).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.status = 5) AND (t.admin_locked = 0) ") + freeT).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(2, model.TaskDelivering))
	mock.ExpectExec(qTaskLease + ".*WHERE `id` = 2 AND").WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.id = 2)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(2, model.TaskSucceeded))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_owner` = NULL, `lease_until` = NULL, `updated_at` = `updated_at` WHERE `id` = 2 AND `lease_owner` = 'inst-a'")).
		WillReturnResult(ok1)
	// 释放后记下轮询结束时刻（lease_until = NOW(3)），供 ListByStatus 轮转
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_until` = NOW(3), `updated_at` = `updated_at` WHERE `id` = 2 AND `lease_owner` IS NULL AND `lease_until` IS NULL")).
		WillReturnResult(ok1)
	f.pollRunning(context.Background())
	f.wg.Wait()
	met(t, mock)
}

// 候选按上次轮询时间轮转：没轮询过的在前，其余按 lease_until 升序。
func TestListByStatusRotates(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY t.lease_until IS NOT NULL ASC, t.lease_until ASC, t.updated_at ASC, t.id ASC LIMIT 100")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := f.repo.Task.ListByStatus(context.Background(), model.TaskRunning, 100); err != nil {
		t.Fatal(err)
	}
	met(t, mock)
}
