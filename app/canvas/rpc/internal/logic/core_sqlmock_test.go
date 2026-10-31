package logic

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"google.golang.org/protobuf/proto"
)

func mockCore(t *testing.T) (*core, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	db := bun.NewDB(sdb, mysqldialect.New())
	c := &core{s: &svc.ServiceContext{Config: testConfig(), Repo: repo.New(db), Gateway: aigateway.New(aigateway.Config{}),
		Java: javatool.New("", 0), Bearers: svc.NewBearerCache(time.Hour)}, now: time.Now}
	return c, mock
}

var canvasCols = []string{"id", "user_id", "workspace_id", "title", "revision", "node_count", "snapshot", "deleted", "created_at", "updated_at"}

const snap2 = `{"nodes":[{"id":"t","type":"text","data":{"text":"x"}},{"id":"m","type":"model","data":{"model":"Banana","status":"done","candidates":["https://c/1.png"]}}],"edges":[{"id":"e","source":"t","target":"m","sourceHandle":"text","targetHandle":"prompt"}]}`

func expectCanvas(mock sqlmock.Sqlmock, rev string) {
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_canvas` AS `c` WHERE (c.id = 'cv_1') AND (c.user_id = 7) AND (c.deleted = 0)")).
		WillReturnRows(sqlmock.NewRows(canvasCols).AddRow("cv_1", 7, 1, "t", rev, 2, `{"nodes":[],"edges":[]}`, 0, time.Now(), time.Now()))
}

func TestSaveCanvasRevisionConflict(t *testing.T) {
	c, mock := mockCore(t)
	expectCanvas(mock, "100-aaaa")
	resp, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: snap2, IfMatchRevision: "99-bbbb"})
	if err != nil || !resp.Conflict || resp.Revision != "100-aaaa" {
		t.Fatalf("revision 不符应返回冲突与服务端版本: %+v %v", resp, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveCanvasCASLostRace(t *testing.T) {
	c, mock := mockCore(t)
	expectCanvas(mock, "100-aaaa")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT `node_id` FROM `afv_canvas_node_run` WHERE (canvas_id = 'cv_1') AND (deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}))
	mock.ExpectExec("UPDATE `afv_canvas` SET .* WHERE \\(id = 'cv_1'\\) AND \\(user_id = 7\\) AND \\(deleted = 0\\) AND \\(revision = '100-aaaa'\\)").
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectCanvas(mock, "101-cccc")
	resp, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: snap2, IfMatchRevision: "100-aaaa"})
	if err != nil || !resp.Conflict || resp.Revision != "101-cccc" {
		t.Fatalf("CAS 没抢到应返回冲突: %+v %v", resp, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveCanvasForce(t *testing.T) {
	c, mock := mockCore(t)
	expectCanvas(mock, "100-aaaa")
	mock.ExpectQuery("SELECT DISTINCT `node_id` FROM `afv_canvas_node_run`").
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow("m"))
	// force：不带 revision 条件；node_count=2；有 run 的 m 节点的 status / candidates 已剥离；标题一起改
	mock.ExpectExec(`UPDATE ` + "`afv_canvas`" + ` SET snapshot = CAST\('[^']*"id":"m"[^']*' AS JSON\), revision = '\d+-[0-9a-f]{4}', node_count = 2, cover_url = NULL, updated_at = NOW\(3\), title = '新标题' WHERE \(id = 'cv_1'\) AND \(user_id = 7\) AND \(deleted = 0\)$`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	resp, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: snap2, IfMatchRevision: "1-old", Force: true, HasTitle: true, Title: "新标题"})
	if err != nil || resp.Conflict || resp.NodeCount != 2 || !regexp.MustCompile(`^\d+-[0-9a-f]{4}$`).MatchString(resp.Revision) {
		t.Fatalf("force 应覆盖保存: %+v %v", resp, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveCanvasRejectsBeforeWrite(t *testing.T) {
	c, mock := mockCore(t)
	expectCanvas(mock, "100-aaaa")
	_, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", IfMatchRevision: "100-aaaa",
		Snapshot: `{"nodes":[{"id":"i","type":"image","data":{"image":"data:image/png;base64,AA"}}],"edges":[]}`})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("dataURL 应 1003: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCanvasFromTemplateRemaps(t *testing.T) {
	c, mock := mockCore(t)
	tpl := `{"nodes":[{"id":"tpl-n1","type":"text","data":{"text":"x"}},{"id":"tpl-n2","type":"model","data":{"model":"Banana"}}],
	 "edges":[{"id":"tpl-e1","source":"tpl-n1","target":"tpl-n2","sourceHandle":"text","targetHandle":"prompt"}]}`
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_canvas_template` AS `t` WHERE (t.id = 3)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "scope", "title", "snapshot", "enabled", "deleted"}).AddRow(3, "official", "模特换装", tpl, 1, 0))
	var inserted string
	mock.ExpectExec("INSERT INTO `afv_canvas`").WillReturnResult(sqlmock.NewResult(0, 1)).WillDelayFor(0)
	doc, err := c.CreateCanvasFromTemplate(context.Background(), &canvas.CreateCanvasFromTemplateReq{UserId: 7, WorkspaceId: 1, TemplateId: 3})
	if err != nil {
		t.Fatal(err)
	}
	inserted = doc.Snapshot
	if strings.Contains(inserted, "tpl-") || doc.Title != "模特换装" || doc.NodeCount != 2 || !strings.HasPrefix(doc.Id, "cv_") {
		t.Fatalf("节点 id 应重映射: %+v", doc)
	}
	d := mustDoc(t, inserted)
	if d.Edges[0].Source != d.Nodes[0].ID || d.Edges[0].Target != d.Nodes[1].ID {
		t.Fatal("内部连线保持")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateNotVisible(t *testing.T) {
	c, mock := mockCore(t)
	mock.ExpectQuery("FROM `afv_canvas_template`").
		WillReturnRows(sqlmock.NewRows([]string{"id", "scope", "user_id", "title", "snapshot", "enabled", "deleted"}).AddRow(4, "user", 99, "别人的", `{}`, 1, 0))
	_, err := c.CreateCanvasFromTemplate(context.Background(), &canvas.CreateCanvasFromTemplateReq{UserId: 7, WorkspaceId: 1, TemplateId: 4})
	if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("别人的用户模板 4001: %v", err)
	}
}

func TestListPromptsVisibility(t *testing.T) {
	cases := []struct {
		in    *canvas.ListPromptsReq
		where string
		rows  *sqlmock.Rows
	}{
		// 不传 vertical：官方（启用）+ 本人
		{&canvas.ListPromptsReq{UserId: 7, Model: "GPT"},
			"WHERE (p.deleted = 0) AND ((p.user_id IS NULL AND p.enabled = 1) OR (p.user_id = 7)) AND (JSON_CONTAINS(p.models, JSON_QUOTE('GPT')))",
			sqlmock.NewRows([]string{"id", "user_id", "title", "models", "tags", "enabled"}).AddRow(1, nil, "官方", `["GPT"]`, `["a"]`, 1).AddRow(2, 7, "我的", `["GPT"]`, nil, 1)},
		// mine：只看本人
		{&canvas.ListPromptsReq{UserId: 7, Vertical: "mine"}, "WHERE (p.deleted = 0) AND (p.user_id = 7)", sqlmock.NewRows([]string{"id"})},
		// ecommerce：只看官方启用
		{&canvas.ListPromptsReq{UserId: 7, Vertical: "ecommerce", Group: "model"},
			"WHERE (p.deleted = 0) AND (p.user_id IS NULL) AND (p.enabled = 1) AND (p.vertical = 'ecommerce') AND (p.group_key = 'model')", sqlmock.NewRows([]string{"id"})},
	}
	for i, tc := range cases {
		c, mock := mockCore(t)
		mock.MatchExpectationsInOrder(false) // ScanAndCount 并发发 SELECT 与 COUNT
		mock.ExpectQuery("^SELECT `p`.* " + regexp.QuoteMeta("FROM `afv_prompt_library_item` AS `p` "+tc.where+" ORDER BY (p.user_id IS NULL) DESC")).WillReturnRows(tc.rows)
		mock.ExpectQuery("^SELECT count\\(\\*\\) " + regexp.QuoteMeta("FROM `afv_prompt_library_item` AS `p` "+tc.where) + "$").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
		resp, err := c.ListPrompts(context.Background(), tc.in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if i == 0 && (resp.Total != 2 || resp.List[0].Owner != "official" || resp.List[1].Owner != "mine" || resp.List[0].Tags[0] != "a") {
			t.Fatalf("%+v", resp)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

func TestPromptOwnership(t *testing.T) {
	c, mock := mockCore(t)
	cols := []string{"id", "user_id", "title", "prompt", "vertical"}
	mock.ExpectQuery("FROM `afv_prompt_library_item`").WillReturnRows(sqlmock.NewRows(cols).AddRow(1, nil, "官方", "x", "general"))
	_, err := c.UpdatePrompt(context.Background(), &canvas.UpsertPromptReq{UserId: 7, Id: 1, Title: "t", Prompt: "p"})
	if code, _, _ := xerr.Parse(err); code != xerr.Conflict {
		t.Fatalf("官方 4002: %v", err)
	}
	mock.ExpectQuery("FROM `afv_prompt_library_item`").WillReturnRows(sqlmock.NewRows(cols).AddRow(2, 99, "别人", "x", "mine"))
	_, err = c.DeletePrompt(context.Background(), &canvas.PromptIdReq{UserId: 7, Id: 2})
	if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("别人的 4001: %v", err)
	}
}

func TestCreatePromptLimits(t *testing.T) {
	c, mock := mockCore(t)
	bad := []*canvas.UpsertPromptReq{
		{UserId: 7, Title: strings.Repeat("标", 101), Prompt: "p"},
		{UserId: 7, Title: "t", Prompt: strings.Repeat("字", 3001)},
		{UserId: 7, Title: "t", Prompt: "p", Tags: strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",")},
		{UserId: 7, Title: "", Prompt: "p"},
	}
	for _, b := range bad {
		if _, err := c.CreatePrompt(context.Background(), b); err == nil {
			t.Fatalf("应拒绝: %+v", b)
		}
	}
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(500))
	_, err := c.CreatePrompt(context.Background(), &canvas.UpsertPromptReq{UserId: 7, Title: "t", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("每人 ≤500: %v", err)
	}
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_prompt_library_item`") + ".*'mine', 'favorite', '\\[\"通用\"\\]'").WillReturnResult(sqlmock.NewResult(12, 1))
	p, err := c.CreatePrompt(context.Background(), &canvas.UpsertPromptReq{UserId: 7, Title: "t", Prompt: "p"})
	if err != nil || p.Id != 12 || p.Owner != "mine" || p.Vertical != "mine" || p.Models[0] != "通用" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestRunNodeIdempotency(t *testing.T) {
	c, mock := mockCore(t)
	req := &canvas.RunNodeRequest{Kind: "image", Model: "Banana", Prompt: "p", Count: 1}
	stored := `{"model":"Banana","model_id":"","kind":"image","prompt":"p","references":[],"ratio":"","size":"","quality":"","count":1,"cfg":0}`
	cols := []string{"id", "canvas_id", "node_id", "user_id", "run_id", "kind", "request", "status", "points"}
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.user_id = 7) AND (r.run_id = 'run-1')")).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(5, "cv_1", "m", 7, "run-1", "image", stored, 1, 40))
	resp, err := c.RunNode(context.Background(), &canvas.RunNodeReq{UserId: 7, CanvasId: "cv_1", NodeId: "m", RunId: "run-1", Request: req})
	if err != nil || !resp.Replayed || resp.RequestId != "5" || resp.Status != "running" || resp.Credits != 40 {
		t.Fatalf("同 key 同 body 重放: %+v %v", resp, err)
	}
	mock.ExpectQuery("WHERE \\(r.user_id = 7\\)").WillReturnRows(sqlmock.NewRows(cols).AddRow(5, "cv_1", "m", 7, "run-1", "image", stored, 1, 40))
	req2 := proto.Clone(req).(*canvas.RunNodeRequest)
	req2.Prompt = "q"
	_, err = c.RunNode(context.Background(), &canvas.RunNodeReq{UserId: 7, CanvasId: "cv_1", NodeId: "m", RunId: "run-1", Request: req2})
	if code, _, _ := xerr.Parse(err); code != xerr.Conflict {
		t.Fatalf("同 key 不同 body 4002: %v", err)
	}
	_, err = c.RunNode(context.Background(), &canvas.RunNodeReq{UserId: 7, CanvasId: "cv_1", NodeId: "m", RunId: "", Request: req})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamMissing {
		t.Fatalf("缺 Idempotency-Key 1002: %v", err)
	}
}

func TestCancelFinishedRun(t *testing.T) {
	c, mock := mockCore(t)
	mock.ExpectQuery("FROM `afv_canvas_node_run`").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "request"}).AddRow(5, 7, 2, `{}`))
	_, err := c.CancelRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if code, _, _ := xerr.Parse(err); code != xerr.Conflict {
		t.Fatalf("已结束 4002: %v", err)
	}
	mock.ExpectQuery("FROM `afv_canvas_node_run`").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "request"}).AddRow(5, 8, 1, `{}`))
	_, err = c.GetRun(context.Background(), &canvas.RunIdReq{UserId: 7, Id: 5})
	if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("别人的 run 4001: %v", err)
	}
}
