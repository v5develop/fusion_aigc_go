package repo

import (
	"context"
	"regexp"
	"strconv"
	"testing"

	"aigc-local-life/app/canvas/model"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
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

var ctx = context.Background()

func TestRunTransitFillsTimes(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET `status` = 1, `updated_at` = NOW(3), `backend_ref` = 'call_1', `started_at` = NOW(3) WHERE (`id` = 3) AND (`status` = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET `status` = 3, `updated_at` = NOW(3), `error_reason` = 'boom', `finished_at` = NOW(3) WHERE (`id` = 3) AND (`status` = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if ok, err := r.Run.Transit(ctx, 3, model.RunQueued, model.RunRunning, map[string]any{"backend_ref": "call_1"}); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := r.Run.Transit(ctx, 3, model.RunRunning, model.RunFailed, map[string]any{"error_reason": "boom"}); ok || err != nil {
		t.Fatalf("没抢到应 ok=false: ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLatestByCanvas(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.id IN (SELECT MAX(id) FROM `afv_canvas_node_run` WHERE (canvas_id = 'cv_1') AND (deleted = 0) GROUP BY node_id))")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "node_id"}).AddRow(9, "m"))
	list, err := r.Run.LatestByCanvas(ctx, "cv_1")
	if err != nil || len(list) != 1 || list[0].NodeID != "m" {
		t.Fatalf("%+v %v", list, err)
	}
}

func TestCanvasSaveCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec("UPDATE `afv_canvas` SET snapshot = CAST\\('\\{\\}' AS JSON\\), revision = '2-b', node_count = 0, cover_url = NULL, updated_at = NOW\\(3\\) WHERE \\(id = 'cv'\\) AND \\(user_id = 1\\) AND \\(deleted = 0\\) AND \\(revision = '1-a'\\)").
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err := r.Canvas.Save(ctx, SaveArgs{ID: "cv", UserID: 1, NewRevision: "2-b", Snapshot: []byte("{}"), IfMatch: "1-a"})
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestActiveOnNodeAndCost(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.canvas_id = 'cv') AND (r.node_id = 'n') AND (r.status IN (5, 0, 1)) ORDER BY r.id DESC LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if x, err := r.Run.ActiveOnNode(ctx, "cv", "n"); x != nil || err != nil {
		t.Fatalf("%v %v", x, err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT r.model_group AS k, COUNT(*) AS runs")).
		WillReturnRows(sqlmock.NewRows([]string{"k", "runs", "succeeded", "failed", "canceled", "cost", "points"}).AddRow("img.x", 3, 2, 1, 0, "0.12", 40))
	rows, err := r.Run.CostSummary(ctx, 0, 0, "model_group")
	if err != nil || len(rows) != 1 || rows[0].Runs != 3 || rows[0].Cost != "0.12" {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestInsertRunBackfillsInSameTx(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO `afv_canvas_node_run`").WillReturnResult(sqlmock.NewResult(42, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET task_id = 'cv_42_run', billing_request_id = 'canvas-cv_42_run' WHERE (id = 42) AND (task_id IS NULL)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	run := &model.NodeRun{CanvasID: "cv", NodeID: "n", UserID: 7, RunID: "k", Kind: model.KindImage, Request: []byte(`{}`), Status: model.RunSubmitting, CostCNY: "0"}
	err := r.InsertRun(ctx, run, func(id int64) (string, string) {
		s := "cv_" + strconv.FormatInt(id, 10) + "_run"
		return s, "canvas-" + s
	})
	if err != nil || run.ID != 42 || run.TaskID.String != "cv_42_run" || run.BillingRequestID.String != "canvas-cv_42_run" {
		t.Fatalf("插入与回填同一事务: %+v %v", run, err)
	}
	// 同节点已有在途 run：uk_active_node 冲突 → ErrActiveOnNode，事务回滚
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO `afv_canvas_node_run`").
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'cv/n' for key 'afv_canvas_node_run.uk_active_node'"})
	mock.ExpectRollback()
	err = r.InsertRun(ctx, &model.NodeRun{CanvasID: "cv", NodeID: "n", Request: []byte(`{}`), CostCNY: "0"}, func(int64) (string, string) { return "x", "" })
	if !IsActiveConflict(err) {
		t.Fatalf("应识别为同节点在途冲突: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLatestSucceededByNode(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.id IN (SELECT MAX(id) FROM `afv_canvas_node_run` WHERE (canvas_id = 'cv') AND (deleted = 0) AND (status = 2) GROUP BY node_id))")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "node_id", "status"}).AddRow(3, "n", 2))
	list, err := r.Run.LatestSucceededByNode(ctx, "cv")
	if err != nil || len(list) != 1 || list[0].ID != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET updated_at = NOW(3) WHERE (id IN (1, 2)) AND (status = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := r.Run.Touch(ctx, []int64{1, 2}, model.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
