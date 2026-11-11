package logic

import (
	"context"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	lrTaskCols = []string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_rid", "result_url", "deleted", "created_at"}
	lrJobCols  = []string{"id", "task_pk", "task_id", "user_id", "src_rid", "ref", "status", "attempt", "next_at", "last_error", "created_at", "updated_at", "finished_at"}
	qLRTask    = regexp.QuoteMeta("WHERE (t.task_id = 'simg_1') LIMIT 1")
	qLRJob     = regexp.QuoteMeta("FROM `afv_studio_copy_job` AS `j` WHERE (j.task_pk = 1) AND (j.ref = '" + paidRef + "') ORDER BY j.id DESC LIMIT 1")
	qLRReset   = regexp.QuoteMeta("UPDATE `afv_studio_copy_job` SET status = 0, attempt = (attempt DIV 1000 + 1) * 1000, next_at = NOW(3), finished_at = NULL " +
		"WHERE (id = 5) AND (status IN (2, 3)) AND ((finished_at IS NULL OR finished_at <= NOW(3) - INTERVAL 60 SECOND))")
	qLRStatus = regexp.QuoteMeta("SELECT `task_pk`, `ref`, `status` FROM `afv_studio_copy_job` WHERE (task_pk IN (1))")
)

func lrTask(userID int64) *sqlmock.Rows {
	return sqlmock.NewRows(lrTaskCols).AddRow(1, 9, userID, "imagegen", "simg_1", model.TaskSucceeded, wmRID, paidRef, 0, time.Now())
}

func lrJob(status int8) *sqlmock.Rows {
	return sqlmock.NewRows(lrJobCols).AddRow(5, 1, "simg_1", 7, wmRID, paidRef, status, 10, time.Now(), "copy failed", time.Now(), time.Now(), time.Now())
}

// expectGetTask 末尾的 GetTask：子任务 → 批次 → 转存状态 → 素材库 asset_id。
func expectGetTask(mock sqlmock.Sqlmock, jobStatus int8) {
	mock.ExpectQuery(qLRTask).WillReturnRows(lrTask(7))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectQuery(qLRStatus).WillReturnRows(sqlmock.NewRows([]string{"task_pk", "ref", "status"}).AddRow(1, paidRef, jobStatus))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
}

func lrLogic(t *testing.T) (*RetryLibraryCopyLogic, sqlmock.Sqlmock, *[]int64) {
	r, mock := newMockRepo(t)
	var kicked []int64
	s := &svc.ServiceContext{Repo: r, KickCopy: func(pk int64) { kicked = append(kicked, pk) }}
	return NewRetryLibraryCopyLogic(context.Background(), s), mock, &kicked
}

func TestStudioRetryLibraryCopyResetsAndKicks(t *testing.T) {
	for _, st := range []int8{model.CopyFailed, model.CopyVoid} {
		l, mock, kicked := lrLogic(t)
		mock.ExpectQuery(qLRTask).WillReturnRows(lrTask(7))
		mock.ExpectQuery(qLRJob).WillReturnRows(lrJob(st))
		mock.ExpectExec(qLRReset).WillReturnResult(sqlmock.NewResult(0, 1))
		expectGetTask(mock, model.CopyPending)
		out, err := l.RetryLibraryCopy(&studio.GetTaskReq{TaskId: " simg_1 ", UserId: 7})
		if err != nil {
			t.Fatalf("status %d: %v", st, err)
		}
		if out.LibraryStatus != LibraryPending {
			t.Errorf("status %d: library_status = %q", st, out.LibraryStatus)
		}
		if len(*kicked) != 1 || (*kicked)[0] != 1 {
			t.Errorf("status %d: kicked = %v", st, *kicked)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStudioRetryLibraryCopyIdempotentWhenPending(t *testing.T) {
	l, mock, kicked := lrLogic(t)
	mock.ExpectQuery(qLRTask).WillReturnRows(lrTask(7))
	mock.ExpectQuery(qLRJob).WillReturnRows(lrJob(model.CopyPending))
	expectGetTask(mock, model.CopyPending)
	out, err := l.RetryLibraryCopy(&studio.GetTaskReq{TaskId: "simg_1", UserId: 7})
	if err != nil || out.LibraryStatus != LibraryPending || len(*kicked) != 0 {
		t.Fatalf("out=%v err=%v kicked=%v", out, err, *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStudioRetryLibraryCopyCooldownAndConcurrent(t *testing.T) {
	// 冷却中：重置没改到、重读仍是失败 → 4002
	l, mock, kicked := lrLogic(t)
	mock.ExpectQuery(qLRTask).WillReturnRows(lrTask(7))
	mock.ExpectQuery(qLRJob).WillReturnRows(lrJob(model.CopyFailed))
	mock.ExpectExec(qLRReset).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(qLRJob).WillReturnRows(lrJob(model.CopyFailed))
	_, err := l.RetryLibraryCopy(&studio.GetTaskReq{TaskId: "simg_1", UserId: 7})
	if code, _, _ := xerr.Parse(err); code != xerr.Conflict || len(*kicked) != 0 {
		t.Fatalf("cooldown: err=%v kicked=%v", err, *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// 并发：另一次已重置（重读是待转存）→ 按成功返回
	l, mock, kicked = lrLogic(t)
	mock.ExpectQuery(qLRTask).WillReturnRows(lrTask(7))
	mock.ExpectQuery(qLRJob).WillReturnRows(lrJob(model.CopyFailed))
	mock.ExpectExec(qLRReset).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(qLRJob).WillReturnRows(lrJob(model.CopyPending))
	expectGetTask(mock, model.CopyPending)
	out, err := l.RetryLibraryCopy(&studio.GetTaskReq{TaskId: "simg_1", UserId: 7})
	if err != nil || out.LibraryStatus != LibraryPending {
		t.Fatalf("concurrent: out=%v err=%v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStudioRetryLibraryCopyOwnership(t *testing.T) {
	l, mock, _ := lrLogic(t)
	mock.ExpectQuery(qLRTask).WillReturnRows(lrTask(8))
	_, err := l.RetryLibraryCopy(&studio.GetTaskReq{TaskId: "simg_1", UserId: 7})
	if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if _, err := l.RetryLibraryCopy(&studio.GetTaskReq{TaskId: " ", UserId: 7}); err == nil {
		t.Fatal("empty task id accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
