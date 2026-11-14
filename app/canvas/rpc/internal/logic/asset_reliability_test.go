package logic

// 付费产物入库可靠性（C1 同步路径 outbox 兜底 / C2 永久失败定时重投 / C3 无 task_id 告警 / 对账补入）。

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/asset"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// assetCore mockCore + 素材库写入（独立 sqlmock）。
func assetCore(t *testing.T) (*core, sqlmock.Sqlmock, sqlmock.Sqlmock) {
	c, mock := mockCore(t)
	adb, amock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adb.Close() })
	c.s.Asset = asset.NewWriter(sqlx.NewSqlConnFromDB(adb))
	c.s.Config.Asset.Enabled = true
	return c, mock, amock
}

func libCands() []model.Candidate {
	return []model.Candidate{{RID: "inst01-asset-dddddddddddddddddddddddddd", URL: "https://cdn/lib0", W: 8, H: 8}}
}

// C1：同步路径入库失败不只打日志——登记一行 outbox（30 秒后对扫描可见），由转存池只做入库重试。
func TestSyncInsertFailureEnqueuesOutbox(t *testing.T) {
	c, mock, amock := assetCore(t)
	run := gatewayRun(5, model.RunSucceeded)
	amock.ExpectBegin()
	amock.ExpectQuery("SELECT COUNT\\(1\\) FROM afv_asset WHERE source_task_id = \\? AND user_id = \\? FOR UPDATE").
		WithArgs("cv_t", int64(7)).WillReturnError(errors.New("Error 1205: Lock wait timeout"))
	amock.ExpectRollback()
	mock.ExpectExec("INSERT IGNORE INTO `afv_canvas_copy_job` .*NOW\\(3\\) \\+ INTERVAL 30 SECOND").WillReturnResult(sqlmock.NewResult(1, 1))
	c.insertAssets(context.Background(), run, libCands(), "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("入库失败应登记 outbox: %v", err)
	}
	if err := amock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// C1：入库成功不登记 outbox；同 task_id 已有行（崩溃在入库与置成功之间后重做）不再写第二组。
func TestSyncInsertOnceNoOutbox(t *testing.T) {
	c, mock, amock := assetCore(t)
	run := gatewayRun(5, model.RunSucceeded)
	amock.ExpectBegin()
	amock.ExpectQuery("SELECT COUNT\\(1\\) FROM afv_asset WHERE source_task_id = \\? AND user_id = \\? FOR UPDATE").
		WithArgs("cv_t", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	amock.ExpectCommit()
	c.insertAssets(context.Background(), run, libCands(), "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := amock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// C1：入库与登记 outbox 都失败（如 Go_V1.0.29 未执行）：不 panic、不影响 run，留给对账扫描（ASSET_RECONCILE_PENDING）。
func TestSyncInsertAndEnqueueBothFail(t *testing.T) {
	c, mock, amock := assetCore(t)
	run := gatewayRun(5, model.RunSucceeded)
	amock.ExpectBegin()
	amock.ExpectQuery("SELECT COUNT\\(1\\) FROM afv_asset").WillReturnError(errors.New("boom"))
	amock.ExpectRollback()
	mock.ExpectExec("INSERT IGNORE INTO `afv_canvas_copy_job`").WillReturnError(errors.New("Error 1146: Table doesn't exist"))
	c.insertAssets(context.Background(), run, libCands(), "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// C1：同步路径登记的 outbox 行（产物已是素材库地址）由转存池执行时不复制，只入库后完成。
func TestCopyJobAssetOnly(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	adb, amock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adb.Close() })
	c.s.Asset = asset.NewWriter(sqlx.NewSqlConnFromDB(adb))
	c.s.Config.Asset.Enabled = true
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", `[{"rid":"inst01-asset-dddddddddddddddddddddddddd","url":"https://cdn/lib0"}]`, nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 1").WillReturnResult(sqlmock.NewResult(0, 1))
	amock.ExpectBegin()
	amock.ExpectQuery("SELECT COUNT\\(1\\) FROM afv_asset").WithArgs("cv_t", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	amock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	amock.ExpectExec("INSERT INTO afv_asset").WillReturnResult(sqlmock.NewResult(11, 1))
	amock.ExpectCommit()
	c.runCopyJob(context.Background(), copyJob(0))
	if lm.copyCount() != 0 {
		t.Fatalf("只入库的行不应复制: %v", lm.copies)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := amock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

var copyJobCols = []string{"id", "run_pk", "task_id", "user_id", "status", "attempt", "next_at", "last_error", "created_at", "updated_at", "finished_at"}

// C2：永久失败行按代退避自动重投：attempt CAS 进下一代 + 清 run 失败标记同事务，交给转存池；并发已被别处重投的跳过。
func TestRequeueFailedCopies(t *testing.T) {
	c, mock, _, kicked := asyncCore(t)
	c.s.Config.Library.AutoRequeueMax = 3
	c.s.Config.Library.AutoRequeueBatch = 10
	now := time.Now()
	mock.ExpectQuery("FROM `afv_canvas_copy_job` AS `j` WHERE \\(j.status = 2\\) AND \\(j.attempt < 3000\\) AND \\(finished_at IS NOT NULL AND finished_at <= NOW\\(3\\) - INTERVAL \\(CASE attempt DIV 1000 WHEN 0 THEN 1800 WHEN 1 THEN 3600 WHEN 2 THEN 7200 ELSE 21600 END\\) SECOND\\) ORDER BY j.finished_at ASC, j.id ASC LIMIT 10").
		WillReturnRows(sqlmock.NewRows(copyJobCols).
			AddRow(9, 5, "cv_t", 7, model.CopyFailed, 10, now, "x", now, now, now).
			AddRow(10, 6, "cv_u", 7, model.CopyFailed, 1004, now, "x", now, now, now))
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 0, attempt = \\(attempt DIV 1000 \\+ 1\\) \\* 1000, next_at = NOW\\(3\\), finished_at = NULL WHERE \\(id = 9\\) AND \\(status = 2\\) AND \\(attempt = 10\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET error_code = NULL WHERE \\(id = 5\\) AND \\(status = 2\\) AND \\(error_code = 'LIBRARY_COPY_FAILED'\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 0.* WHERE \\(id = 10\\) AND \\(status = 2\\) AND \\(attempt = 1004\\)").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if n := c.requeueFailedCopies(context.Background()); n != 1 {
		t.Fatalf("应重投 1 行: %d", n)
	}
	if len(*kicked) != 1 || (*kicked)[0] != 5 {
		t.Fatalf("重投的行应交给转存池: %v", *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// C2：AutoRequeueMax=0 关闭自动重投（不查库）；代数判定（用尽才告警 EXHAUSTED）。
func TestRequeueDisabledAndGenerations(t *testing.T) {
	c, mock, _, _ := asyncCore(t)
	if n := c.requeueFailedCopies(context.Background()); n != 0 {
		t.Fatal("关闭时不应重投")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	c.s.Config.Library.AutoRequeueMax = 2
	for attempt, want := range map[int32]bool{10: true, 1010: true, 2010: false, 5003: false} {
		if got := c.autoRequeueLeft(attempt); got != want {
			t.Errorf("attempt %d: autoRequeueLeft=%v want %v", attempt, got, want)
		}
	}
}

// C2：失败标记优先——产物已是素材库地址、只差入库的 run 永久失败也给 failed（用户可一键重试）。
func TestLibraryStatusFailedWithoutPendingRefs(t *testing.T) {
	r := gatewayRun(5, model.RunSucceeded)
	r.Candidates = []byte(`[{"url":"https://cdn/lib0"}]`)
	if s := libraryStatus(r); s != "" {
		t.Fatalf("已在素材库应为空: %q", s)
	}
	r.ErrorCode = sql.NullString{String: model.CopyFailedCode, Valid: true}
	if s := libraryStatus(r); s != LibraryFailed {
		t.Fatalf("有失败标记应为 failed: %q", s)
	}
}

// C3：没有 task_id 的 run 不入库（无从幂等），也不拿主键凑 source_task_id。
func TestAssetListWithoutTaskID(t *testing.T) {
	c, _, _ := assetCore(t)
	run := gatewayRun(5, model.RunSucceeded)
	run.TaskID = sql.NullString{}
	if list := c.assetList(run, libCands(), ""); list != nil {
		t.Fatalf("无 task_id 不应入库: %+v", list)
	}
}

var reconRunCols = []string{"id", "user_id", "kind", "backend", "status", "billing_status", "task_id", "request", "candidates", "video_url", "updated_at"}

// 对账：成功且已结算、素材库无行的 run 补入；已有行的跳过；仍是 mr:// 的不归对账。
func TestCanvasReconcile(t *testing.T) {
	c, mock, amock := assetCore(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	rec := c.newAssetReconciler()
	rec.Conf = asset.ReconcileConf{Enabled: true, WindowHours: 168, GraceSeconds: 600, Batch: 10}
	t1 := now.Add(-time.Hour)
	mock.ExpectQuery("FROM `afv_canvas_node_run` AS `r` WHERE \\(r.status = 2\\) AND \\(r.billing_status = 'settled'\\) AND \\(r.kind IN \\('image', 'video', 'tool_crop'\\)\\) AND \\(r.updated_at >= .* AND r.updated_at < .*\\) ORDER BY r.updated_at ASC, r.id ASC LIMIT 10").
		WillReturnRows(sqlmock.NewRows(reconRunCols).
			AddRow(1, 7, "image", "gateway", 2, "settled", "cv_a", `{}`, `[{"url":"https://cdn/a"}]`, nil, t1).
			AddRow(2, 7, "image", "gateway", 2, "settled", "cv_b", `{}`, `[{"url":"https://cdn/b"}]`, nil, t1).
			AddRow(3, 7, "image", "gateway", 2, "settled", "cv_c", `{}`, refCands(srcRID0), nil, t1))
	amock.ExpectQuery("SELECT DISTINCT source_task_id FROM afv_asset WHERE source_task_id IN \\(\\?,\\?\\)").WithArgs("cv_a", "cv_b").
		WillReturnRows(sqlmock.NewRows([]string{"source_task_id"}).AddRow("cv_b"))
	amock.ExpectBegin()
	amock.ExpectQuery("SELECT COUNT\\(1\\) FROM afv_asset .* FOR UPDATE").WithArgs("cv_a", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	amock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	amock.ExpectExec("INSERT INTO afv_asset").WithArgs(int64(7), int64(3), "cv_a", "image", "canvas_image",
		sqlmock.AnyArg(), "https://cdn/a", "https://cdn/a", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(11, 1))
	amock.ExpectCommit()
	st := rec.RunOnce(context.Background())
	if st.Scanned != 3 || st.Missing != 1 || st.Inserted != 1 || st.Failed != 0 {
		t.Fatalf("对账计数不符: %+v", st)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := amock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
