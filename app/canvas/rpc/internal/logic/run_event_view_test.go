package logic

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"

	"github.com/DATA-DOG/go-sqlmock"
)

// run 事件载荷测试用的行（只列 toResult 用到的列，其余零值）。
var evRunCols = []string{"id", "canvas_id", "node_id", "user_id", "run_id", "kind", "request", "backend", "status", "progress",
	"candidates", "selected_index", "video_url", "requested_count", "points", "billing_status", "error_code", "error_reason",
	"cancel_request", "attempt", "created_at", "started_at", "finished_at", "updated_at"}

type runEvRow struct {
	status, cancel  int8
	candidates      string
	errCode, errMsg any
	started, finish any
	billing         string
}

// evNow 固定的创建时间：同一用例里两次取行（事件 / GetRun）的 created_at 一致。
var evNow = time.Now()

func evRunRows(r runEvRow) *sqlmock.Rows {
	now := evNow
	billing := r.billing
	if billing == "" {
		billing = model.BillHeld
	}
	var cands any
	if r.candidates != "" {
		cands = r.candidates
	}
	return sqlmock.NewRows(evRunCols).AddRow(9, "cv_1", "m_1", 7, "run-abc", "image", `{"model":"Banana"}`, "gateway", r.status, 5,
		cands, 1, nil, 2, 20, billing, r.errCode, r.errMsg, r.cancel, 1, now.Add(-30*time.Second), r.started, r.finish, now)
}

func expectRunReload(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery("FROM `afv_canvas_node_run` AS `r` WHERE \\(r.id = 9\\)").WillReturnRows(rows)
}

func expectRevisionOf(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `revision` FROM `afv_canvas` WHERE (id = 'cv_1')")).
		WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow("500-abcd"))
}

func evCore(t *testing.T) (*core, sqlmock.Sqlmock, *eventSink) {
	t.Helper()
	c, mock := mockCore(t)
	pub, sub := newSink(t, "cv_1")
	c.s.CanvasEvents = pub
	return c, mock, sub("cv_1")
}

// 事件里的结果与 GetRun 同一口径：调用方手里只有部分字段的 run，发布前按 id 重读库。
func TestPublishRunPayloadMatchesGetRun(t *testing.T) {
	c, mock, sink := evCore(t)
	row := runEvRow{status: model.RunSucceeded, candidates: `[{"url":"https://cdn/a.png","w":1024,"h":768},{"url":"https://cdn/b.png","w":1024,"h":768}]`,
		started: evNow.Add(-20 * time.Second), finish: evNow, billing: model.BillSettled}
	expectRunReload(mock, evRunRows(row))
	expectRevisionOf(mock)
	c.publishRun(context.Background(), &model.NodeRun{ID: 9, CanvasID: "cv_1", NodeID: "m_1", Status: model.RunRunning}) // 内存里还是旧状态
	e := sink.next(t)
	if e.Run == nil || e.RunStatus != "succeeded" {
		t.Fatalf("应带重读后的结果: %+v", e)
	}
	expectRunReload(mock, evRunRows(row))
	got, err := c.GetRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 9})
	if err != nil {
		t.Fatal(err)
	}
	want := runViewOf(got)
	if !sameView(e.Run, want) {
		t.Fatalf("事件结果与 GetRun 不一致:\nevent=%+v\nget  =%+v", e.Run, want)
	}
	if e.Run.Version != evNow.UnixMilli() {
		t.Fatalf("version 应为行的 updated_at 毫秒: %d", e.Run.Version)
	}
	if len(e.Run.Candidates) != 2 || e.Run.Candidates[0].URL != "https://cdn/a.png" || e.Run.SelectedIndex != 1 || e.Run.Progress != 100 ||
		e.Run.ElapsedMs != 0 || e.Run.ExpectedMs != 0 {
		t.Fatalf("成功载荷: %+v", e.Run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 运行中：带本地外推参数；取消被拒的 cancel_state 一并下发。
func TestPublishRunRunningCarriesProgressHints(t *testing.T) {
	c, mock, sink := evCore(t)
	expectRunReload(mock, evRunRows(runEvRow{status: model.RunRunning, cancel: model.CancelRefused, started: time.Now().Add(-6 * time.Second)}))
	expectRevisionOf(mock)
	c.publishRun(context.Background(), &model.NodeRun{ID: 9, CanvasID: "cv_1"})
	e := sink.next(t)
	if e.Run == nil || e.Run.Status != "running" || e.Run.CancelState != "refused" || e.Run.ExpectedMs != 60_000 ||
		e.Run.ElapsedMs < 5_000 || e.Run.Progress < 5 || e.Run.Progress > 95 {
		t.Fatalf("运行中载荷: %+v", e.Run)
	}
}

// 重读失败：照发状态提示（前端收到没有 run 的事件就立即 GET 一次），不影响业务。
func TestPublishRunReloadFailureStillHints(t *testing.T) {
	c, mock, sink := evCore(t)
	mock.ExpectQuery("FROM `afv_canvas_node_run` AS `r` WHERE \\(r.id = 9\\)").WillReturnError(errors.New("db down"))
	expectRevisionOf(mock)
	c.publishRun(context.Background(), &model.NodeRun{ID: 9, CanvasID: "cv_1", NodeID: "m_1", Status: model.RunQueued})
	e := sink.next(t)
	if e.Run != nil || e.RunStatus != "queued" || e.RequestID != "9" {
		t.Fatalf("应只发状态提示: %+v", e)
	}
}

// 转换点：排队 → 提交中（claimDispatch）、提交中 → 退回排队（releaseClaim）、请求取消（cancel_state=requested）都发事件。
func TestRunTransitionsPublish(t *testing.T) {
	ctx := context.Background()
	t.Run("claimDispatch", func(t *testing.T) {
		c, mock, sink := evCore(t)
		mock.ExpectExec("UPDATE `afv_canvas_node_run` SET status = 5").WillReturnResult(sqlmock.NewResult(0, 1))
		expectRunReload(mock, evRunRows(runEvRow{status: model.RunSubmitting}))
		expectRevisionOf(mock)
		if !c.claimDispatch(ctx, &model.NodeRun{ID: 9, CanvasID: "cv_1", Status: model.RunQueued}, "") {
			t.Fatal("应抢到")
		}
		if e := sink.next(t); e.Run == nil || e.Run.Status != "submitting" {
			t.Fatalf("提交中事件: %+v", e)
		}
	})
	t.Run("releaseClaim", func(t *testing.T) {
		c, mock, sink := evCore(t)
		mock.ExpectExec("UPDATE `afv_canvas_node_run`").WillReturnResult(sqlmock.NewResult(0, 1))
		expectRunReload(mock, evRunRows(runEvRow{status: model.RunQueued}))
		expectRevisionOf(mock)
		c.releaseClaim(ctx, &model.NodeRun{ID: 9, CanvasID: "cv_1", Status: model.RunSubmitting, Attempt: 1}, "")
		if e := sink.next(t); e.Run == nil || e.Run.Status != "queued" {
			t.Fatalf("退回排队事件: %+v", e)
		}
	})
	t.Run("requestCancel", func(t *testing.T) {
		c, mock, sink := evCore(t)
		c.cancelWait = 0
		mock.ExpectExec("UPDATE `afv_canvas_node_run` SET cancel_request = 1").WillReturnResult(sqlmock.NewResult(0, 1))
		expectRunReload(mock, evRunRows(runEvRow{status: model.RunSubmitting, cancel: model.CancelRequested}))
		expectRevisionOf(mock)
		expectRunReload(mock, evRunRows(runEvRow{status: model.RunSubmitting, cancel: model.CancelRequested})) // awaitCancel
		if _, _, err := c.requestCancel(ctx, &model.NodeRun{ID: 9, CanvasID: "cv_1", Status: model.RunSubmitting}, 7); err != nil {
			t.Fatal(err)
		}
		if e := sink.next(t); e.Run == nil || e.Run.CancelState != "requested" || e.Run.Status != "submitting" {
			t.Fatalf("取消中事件: %+v", e)
		}
	})
	t.Run("endRun", func(t *testing.T) {
		c, mock, sink := evCore(t)
		mock.ExpectExec("UPDATE `afv_canvas_node_run`").WillReturnResult(sqlmock.NewResult(0, 1))
		expectRunReload(mock, evRunRows(runEvRow{status: model.RunFailed, errCode: "PARAM_INVALID", errMsg: "参数不对", billing: model.BillRefunded,
			finish: time.Now()}))
		expectRevisionOf(mock)
		run := &model.NodeRun{ID: 9, CanvasID: "cv_1", NodeID: "m_1", UserID: 7, Status: model.RunRunning, Kind: model.KindImage}
		if !c.endRun(ctx, run, model.RunRunning, model.RunFailed, "PARAM_INVALID", "参数不对", false, false) {
			t.Fatal("endRun 应抢到")
		}
		e := sink.next(t)
		if e.Run == nil || e.Run.Status != "failed" || e.Run.Error != "参数不对" || e.Run.ErrorCode != "PARAM_INVALID" || e.Run.Retryable ||
			e.Run.Credits != 0 {
			t.Fatalf("失败事件: %+v", e.Run)
		}
	})
}

func sameView(a, b *canvasevent.RunView) bool {
	x, y := *a, *b
	x.ElapsedMs, x.ExpectedMs, y.ElapsedMs, y.ExpectedMs = 0, 0, 0, 0
	x.Version, y.Version = 0, 0 // 事件独有
	return reflect.DeepEqual(x, y)
}
