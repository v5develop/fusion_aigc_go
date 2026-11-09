package repo

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

func newMock(t *testing.T) (*Repo, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return New(bun.NewDB(sdb, mysqldialect.New())), mock
}

func done(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

var ctx = context.Background()

func TestListHistoryCursor(t *testing.T) {
	r, mock := newMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_studio_batch` AS `b` WHERE (b.user_id = 7) AND (b.deleted = 0) AND (b.kind = 'tryon') " +
		"AND ((b.created_at < FROM_UNIXTIME(1730000000123 / 1000) OR (b.created_at = FROM_UNIXTIME(1730000000123 / 1000) AND b.id < 99))) " +
		"ORDER BY b.created_at DESC, b.id DESC LIMIT 21")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "kind", "payload", "created_at"}).
			AddRow(98, 7, "tryon", `{"a":1}`, now))
	list, err := r.Batch.ListHistory(ctx, 7, model.KindTryon, Cursor{Ms: 1730000000123, ID: 99}, 21)
	if err != nil || len(list) != 1 || list[0].ID != 98 || string(list[0].Payload) != `{"a":1}` {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	done(t, mock)

	// 第一页不带游标条件
	r, mock = newMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.user_id = 7) AND (b.deleted = 0) ORDER BY b.created_at DESC, b.id DESC LIMIT 5")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := r.Batch.ListHistory(ctx, 7, "", Cursor{}, 5); err != nil {
		t.Fatal(err)
	}
	done(t, mock)
}

func TestBatchCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 1, `updated_at` = NOW(3) WHERE (`id` = 5) AND (`status` = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err := r.Batch.UpdateStatus(ctx, 5, model.BatchQueued, model.BatchCropping, nil)
	if err != nil || ok {
		t.Fatalf("没抢到应 ok=false：ok=%v err=%v", ok, err)
	}
	done(t, mock)
}

func TestTaskTransitFillsTimes(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 1, `updated_at` = NOW(3), `gateway_call_id` = 'c1', `started_at` = NOW(3) WHERE (`id` = 3) AND (`status` = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 4, `updated_at` = NOW(3), `error_reason` = '已取消', `finished_at` = NOW(3) WHERE (`id` = 3) AND (`status` = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.Task.TransitStatus(ctx, 3, model.TaskQueued, model.TaskRunning, map[string]any{"gateway_call_id": "c1"}); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := r.Task.TransitStatus(ctx, 3, model.TaskQueued, model.TaskCanceled, map[string]any{"error_reason": "已取消"}); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	done(t, mock)
}

func TestCacheUpsert(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_prompt_cache` (`id`, `batch_id`, `cache_key`, `kind`, `payload`, `created_at`) " +
		"VALUES (DEFAULT, 9, 'sha1x', 'analyze', '{\"p\":1}', DEFAULT) ON DUPLICATE KEY UPDATE `payload` = VALUES(`payload`), `created_at` = VALUES(`created_at`)")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := r.Cache.Put(ctx, 9, "sha1x", model.CacheAnalyze, json.RawMessage(`{"p":1}`)); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (pc.batch_id = 9) AND (pc.cache_key = 'sha1x') AND (pc.kind = 'analyze') LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"payload"}))
	if _, ok, err := r.Cache.Get(ctx, 9, "sha1x", model.CacheAnalyze); ok || err != nil {
		t.Fatalf("未命中应 ok=false：ok=%v err=%v", ok, err)
	}
	done(t, mock)
}

func TestBulkInsertAndBackfill(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_task` (") + ".*" +
		regexp.QuoteMeta("VALUES (DEFAULT, 1, 7, 'tryon', 0, 0, 0, NULL, 0, 'g', DEFAULT, NULL, DEFAULT, NULL, NULL, 0, 0, NULL, NULL, 0, 0, DEFAULT, NULL, 0, NULL, DEFAULT, NULL, NULL, 0, DEFAULT, DEFAULT), "+
			"(DEFAULT, 1, 7, 'tryon', 0, 1, 0, NULL, 0, 'g', DEFAULT, NULL, DEFAULT, NULL, NULL, 0, 0, NULL, NULL, 0, 0, DEFAULT, NULL, 0, NULL, DEFAULT, NULL, NULL, 0, DEFAULT, DEFAULT)")).
		WillReturnResult(sqlmock.NewResult(10, 2))
	tasks := []*model.Task{
		{BatchID: 1, UserID: 7, Kind: model.KindTryon, ModelGroup: "g"},
		{BatchID: 1, UserID: 7, Kind: model.KindTryon, ShotIndex: 1, ModelGroup: "g"},
	}
	if err := r.Task.BulkInsert(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET task_id = CASE id WHEN 10 THEN 'tyo_a' WHEN 11 THEN 'tyo_b' END, " +
		"billing_request_id = CASE id WHEN 10 THEN 'studio-tyo_a' WHEN 11 THEN 'studio-tyo_b' END WHERE (id IN (10, 11)) AND (task_id IS NULL)")).
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := r.Task.BackfillTaskIDs(ctx, map[int64]string{11: "tyo_b", 10: "tyo_a"}, "studio-"); err != nil {
		t.Fatal(err)
	}
	done(t, mock)
}

func TestAppendStepLog(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET step_log = JSON_ARRAY_APPEND(COALESCE(step_log, JSON_ARRAY()), '$', CAST('{\"step\":\"submitted\",\"ms\":3,\"task_total\":6,\"at\":1}' AS JSON)) WHERE (id = 2)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Batch.AppendStepLog(ctx, 2, model.StepLog{Step: "submitted", Ms: 3, TaskTotal: 6, At: 1}); err != nil {
		t.Fatal(err)
	}
	done(t, mock)
}

func TestTxRebinds(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO `afv_studio_batch`").WillReturnResult(sqlmock.NewResult(42, 1))
	mock.ExpectCommit()
	b := &model.Batch{UserID: 1, Kind: model.KindPose, RequestID: "r", Payload: json.RawMessage(`{}`)}
	err := r.RunInTx(ctx, func(ctx context.Context, tx *Repo) error { return tx.Batch.Insert(ctx, b) })
	if err != nil || b.ID != 42 {
		t.Fatalf("id=%d err=%v", b.ID, err)
	}
	done(t, mock)
}

// 写调用号时记 started_at（真正提交 gateway 的时刻）：started_at 必须排在 gateway_call_id 之前（MySQL 按书写顺序求值，读旧调用号）。
func TestSetGatewayStartedAt(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET gateway_status = 'QUEUED', started_at = IF(gateway_call_id <=> 'c1', COALESCE(started_at, NOW(3)), NOW(3)), gateway_call_id = 'c1' WHERE (id = 5)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET gateway_status = 'RUNNING' WHERE (id = 5)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Task.SetGateway(ctx, 5, "c1", "QUEUED"); err != nil {
		t.Fatal(err)
	}
	if err := r.Task.SetGateway(ctx, 5, "", "RUNNING"); err != nil {
		t.Fatal(err)
	}
	done(t, mock)
}

// 预览缓存清理只带 batch_id < 0 的条件与 LIMIT，且返回影响行数；参数非法不发 SQL。
func TestCacheDeleteStalePreview(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM afv_studio_prompt_cache WHERE (batch_id < 0) AND (created_at < DATE_SUB(NOW(3), INTERVAL 86400 SECOND)) LIMIT 500")).
		WillReturnResult(sqlmock.NewResult(0, 37))
	n, err := r.Cache.DeleteStalePreview(ctx, 86400, 500)
	if err != nil || n != 37 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := r.Cache.DeleteStalePreview(ctx, 0, 500); err == nil {
		t.Fatal("olderThanSec<=0 应报错")
	}
	if _, err := r.Cache.DeleteStalePreview(ctx, 60, 0); err == nil {
		t.Fatal("limit<=0 应报错")
	}
	done(t, mock)
}
