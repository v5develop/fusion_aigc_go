package logic

import (
	"context"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

var retryJobCols = []string{"id", "run_pk", "task_id", "user_id", "status", "attempt", "next_at", "last_error", "created_at", "updated_at", "finished_at"}

const qRetryRun = "WHERE \\(r.id = 5\\) LIMIT 1"

// expectRetryRun 成功 run（候选是待转存的 mr:// 引用），errorCode 为 nil 或 LIBRARY_COPY_FAILED。
func expectRetryRun(mock sqlmock.Sqlmock, userID int64, errorCode any) {
	mock.ExpectQuery(qRetryRun).WillReturnRows(sqlmock.NewRows(copyRunCols).
		AddRow(5, "cv_1", "m", userID, "image", "gateway", model.RunSucceeded, model.BillSettled, 1, callA, "cv_t",
			`{"model":"Banana"}`, refCands(srcRID0), nil, errorCode, 0, time.Now()))
}

func expectRetryJob(mock sqlmock.Sqlmock, status int8) {
	mock.ExpectQuery("FROM `afv_canvas_copy_job` AS `j` WHERE \\(j.run_pk = 5\\) LIMIT 1").WillReturnRows(sqlmock.NewRows(retryJobCols).
		AddRow(9, 5, "cv_t", 7, status, 10, time.Now(), "copy failed", time.Now(), time.Now(), time.Now()))
}

const (
	qResetJob = "UPDATE `afv_canvas_copy_job` SET status = 0, attempt = \\(attempt DIV 1000 \\+ 1\\) \\* 1000, next_at = NOW\\(3\\), finished_at = NULL " +
		"WHERE \\(id = 9\\) AND \\(status = 2\\) AND \\(\\(finished_at IS NULL OR finished_at <= NOW\\(3\\) - INTERVAL 60 SECOND\\)\\)"
	qClearFailed = "UPDATE `afv_canvas_node_run` SET error_code = NULL WHERE \\(id = 5\\) AND \\(status = 2\\) AND \\(error_code = 'LIBRARY_COPY_FAILED'\\)"
)

func TestRetryLibraryCopyResetsFailedJobAndKicks(t *testing.T) {
	c, mock, _, kicked := asyncCore(t)
	expectRetryRun(mock, 7, model.CopyFailedCode)
	expectRetryJob(mock, model.CopyFailed)
	mock.ExpectBegin()
	mock.ExpectExec(qResetJob).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qClearFailed).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectRetryRun(mock, 7, nil) // 重读：失败标记已清

	out, err := c.RetryLibraryCopy(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if err != nil {
		t.Fatal(err)
	}
	if out.LibraryStatus != LibraryPending {
		t.Errorf("library_status = %q, want pending", out.LibraryStatus)
	}
	if len(*kicked) != 1 || (*kicked)[0] != 5 {
		t.Errorf("copier not kicked: %v", *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryLibraryCopyIdempotentWhenNotFailed(t *testing.T) {
	c, mock, _, kicked := asyncCore(t)
	expectRetryRun(mock, 7, nil) // pending：不查 outbox、不写
	out, err := c.RetryLibraryCopy(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if err != nil || out.LibraryStatus != LibraryPending {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if len(*kicked) != 0 {
		t.Errorf("kicked on idempotent call: %v", *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryLibraryCopyOwnership(t *testing.T) {
	c, mock, _, _ := asyncCore(t)
	expectRetryRun(mock, 8, model.CopyFailedCode) // 别人的 run
	_, err := c.RetryLibraryCopy(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryLibraryCopyCooldown(t *testing.T) {
	c, mock, _, kicked := asyncCore(t)
	expectRetryRun(mock, 7, model.CopyFailedCode)
	expectRetryJob(mock, model.CopyFailed)
	mock.ExpectBegin()
	mock.ExpectExec(qResetJob).WillReturnResult(sqlmock.NewResult(0, 0)) // 冷却中：没改到
	mock.ExpectRollback()
	expectRetryJob(mock, model.CopyFailed) // 重读：仍是永久失败 → 限频
	_, err := c.RetryLibraryCopy(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || msg == "" {
		t.Fatalf("err = %v, want Conflict 太频繁", err)
	}
	if len(*kicked) != 0 {
		t.Errorf("kicked during cooldown: %v", *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryLibraryCopyConcurrentResetIsSuccess(t *testing.T) {
	c, mock, _, kicked := asyncCore(t)
	expectRetryRun(mock, 7, model.CopyFailedCode)
	expectRetryJob(mock, model.CopyFailed)
	mock.ExpectBegin()
	mock.ExpectExec(qResetJob).WillReturnResult(sqlmock.NewResult(0, 0)) // 另一次重试先改到
	mock.ExpectRollback()
	expectRetryJob(mock, model.CopyPending)
	expectRetryRun(mock, 7, nil)
	out, err := c.RetryLibraryCopy(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if err != nil || out.LibraryStatus != LibraryPending {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if len(*kicked) != 1 {
		t.Errorf("kicked = %v", *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryLibraryCopyVoidJobConflict(t *testing.T) {
	c, mock, _, _ := asyncCore(t)
	expectRetryRun(mock, 7, model.CopyFailedCode)
	expectRetryJob(mock, model.CopyVoid)
	_, err := c.RetryLibraryCopy(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if code, _, _ := xerr.Parse(err); code != xerr.Conflict {
		t.Fatalf("err = %v, want Conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
