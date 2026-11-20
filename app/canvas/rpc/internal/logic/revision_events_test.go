package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func evDoc(t *testing.T, s string) *Doc {
	t.Helper()
	d, err := ParseSnapshot([]byte(s), 0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestChangedNodeIDs(t *testing.T) {
	base := `{"nodes":[{"id":"a","type":"text","position":{"x":1,"y":2},"data":{"text":"x","n":1}},{"id":"b","type":"image","data":{}},{"id":"c","type":"text","data":{}}],
"edges":[{"id":"e1","source":"a","target":"b"}]}`
	cases := []struct {
		name, cur string
		want      []string
	}{
		{"键序 / 空白 / 数字写法不同不算改", `{"edges":[{"target":"b","source":"a","id":"e1"}],"nodes":[{"data":{"n":1.0,"text":"x"},"position":{"y":2,"x":1},"type":"text","id":"a"},{"id":"b","type":"image","data":{}},{"id":"c","type":"text","data":{}}]}`, []string{}},
		{"移动", `{"nodes":[{"id":"a","type":"text","position":{"x":5,"y":2},"data":{"text":"x","n":1}},{"id":"b","type":"image","data":{}},{"id":"c","type":"text","data":{}}],"edges":[{"id":"e1","source":"a","target":"b"}]}`, []string{"a"}},
		{"新增 + 删除", `{"nodes":[{"id":"a","type":"text","position":{"x":1,"y":2},"data":{"text":"x","n":1}},{"id":"b","type":"image","data":{}},{"id":"d","type":"text","data":{}}],"edges":[{"id":"e1","source":"a","target":"b"}]}`, []string{"d", "c"}},
		{"连线改端点：新旧两端都算", `{"nodes":[{"id":"a","type":"text","position":{"x":1,"y":2},"data":{"text":"x","n":1}},{"id":"b","type":"image","data":{}},{"id":"c","type":"text","data":{}}],"edges":[{"id":"e1","source":"c","target":"b"}]}`, []string{"c", "b", "a"}},
		{"删线", `{"nodes":[{"id":"a","type":"text","position":{"x":1,"y":2},"data":{"text":"x","n":1}},{"id":"b","type":"image","data":{}},{"id":"c","type":"text","data":{}}],"edges":[]}`, []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := changedNodeIDs(evDoc(t, base), evDoc(t, c.cur)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
	if got := changedNodeIDs(nil, evDoc(t, base)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("新建画布应列出全部节点: %v", got)
	}
}

func TestEventClipAndOlder(t *testing.T) {
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i)
	}
	e := canvasevent.Event{ChangedNodeIDs: ids}
	e.Clip()
	if len(e.ChangedNodeIDs) != 200 || !e.Truncated {
		t.Fatal("应截断到 200 并标记")
	}
	if !canvasevent.Older("100-aaaa", "200-bbbb") || canvasevent.Older("200-bbbb", "100-aaaa") || canvasevent.Older("x", "200-b") {
		t.Fatal("Older")
	}
}

// eventSink 订阅一张画布的事件（miniredis）。
type eventSink struct {
	ps *redis.PubSub
	ch <-chan *redis.Message
}

func newSink(t *testing.T, canvasID string) (*canvasevent.Publisher, func(string) *eventSink) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return canvasevent.NewPublisher(rdb), func(id string) *eventSink {
		ps := rdb.Subscribe(context.Background(), canvasevent.Channel(id))
		if _, err := ps.Receive(context.Background()); err != nil { // 等 SUBSCRIBE 确认
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ps.Close() })
		return &eventSink{ps: ps, ch: ps.Channel()}
	}
}

func (s *eventSink) next(t *testing.T) canvasevent.Event {
	t.Helper()
	select {
	case m := <-s.ch:
		var e canvasevent.Event
		if err := json.Unmarshal([]byte(m.Payload), &e); err != nil {
			t.Fatal(err)
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("没收到事件")
	}
	return canvasevent.Event{}
}

func (s *eventSink) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.ch:
		t.Fatalf("不应有事件: %s", m.Payload)
	case <-time.After(80 * time.Millisecond):
	}
}

// 失败 / 取消路径（endRun）发 run 事件：revision 取画布当前值，revision 本身不变。
func TestEndRunPublishesRunEvent(t *testing.T) {
	c, mock := mockCore(t)
	pub, sub := newSink(t, "cv_1")
	c.s.CanvasEvents = pub
	sink := sub("cv_1")
	mock.ExpectExec("UPDATE `afv_canvas_node_run`").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunReload(mock, evRunRows(runEvRow{status: model.RunCanceled, errCode: "CANCELED", errMsg: "已取消执行"})) // 发布前重读（run_event_view.go）
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `revision` FROM `afv_canvas` WHERE (id = 'cv_1')")).
		WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow("500-abcd"))
	run := &model.NodeRun{ID: 9, CanvasID: "cv_1", NodeID: "m_1", UserID: 7, Status: model.RunRunning, Kind: model.KindImage}
	if !c.endRun(context.Background(), run, model.RunRunning, model.RunCanceled, "CANCELED", "已取消执行", false, false) {
		t.Fatal("endRun 应抢到")
	}
	e := sink.next(t)
	if e.Source != canvasevent.SourceRun || e.Revision != "500-abcd" || e.RequestID != "9" || e.RunStatus != model.StatusText(model.RunCanceled) ||
		e.Actor != "run:9" || !reflect.DeepEqual(e.ChangedNodeIDs, []string{"m_1"}) {
		t.Fatalf("run 事件: %+v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 用户保存：发 source=user 事件，changedNodeIds 为存储态差异；没配 Redis 时不算差异、不发。
func TestSaveCanvasPublishesUserEvent(t *testing.T) {
	c, mock := mockCore(t)
	pub, sub := newSink(t, "cv_1")
	c.s.CanvasEvents = pub
	sink := sub("cv_1")
	expectCanvas(mock, "100-aaaa")
	mock.ExpectQuery("SELECT DISTINCT `node_id` FROM `afv_canvas_node_run`").WillReturnRows(sqlmock.NewRows([]string{"node_id"}))
	mock.ExpectExec("UPDATE `afv_canvas` SET").WillReturnResult(sqlmock.NewResult(0, 1))
	resp, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: snap2, IfMatchRevision: "100-aaaa"})
	if err != nil || resp.Conflict {
		t.Fatal(resp, err)
	}
	e := sink.next(t)
	if e.Source != canvasevent.SourceUser || e.Actor != "user:7" || e.Revision != resp.Revision || !reflect.DeepEqual(e.ChangedNodeIDs, []string{"t", "m"}) {
		t.Fatalf("保存事件: %+v", e)
	}
	// 冲突不发
	expectCanvas(mock, "101-bbbb")
	if r, _ := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: snap2, IfMatchRevision: "100-aaaa"}); !r.Conflict {
		t.Fatal("应冲突")
	}
	sink.none(t)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 真库：新建 / 用户保存 / 智能体写入（含服务间 actor）都发事件并记历史；历史按画布裁剪到 RevisionKeep；?revision= 读历史。
func TestRevisionHistoryAndEventsE2E(t *testing.T) {
	c := e2eCore(t)
	c.s.Config.Limits.RevisionKeep = 3
	pub, sub := newSink(t, "")
	c.s.CanvasEvents = pub
	ctx := context.Background()

	doc, err := c.CreateCanvas(ctx, &canvas.CreateCanvasReq{UserId: 7001, Title: "同步"})
	if err != nil {
		t.Fatal(err)
	}
	sink := sub(doc.Id)
	revs := []string{doc.Revision}
	cur := doc.Revision
	for i := 0; i < 4; i++ {
		snap := fmt.Sprintf(`{"nodes":[{"id":"t_%d","type":"text","position":{"x":%d,"y":0},"data":{"kind":"text","text":"v%d"}}],"edges":[]}`, i, i*10, i)
		time.Sleep(2 * time.Millisecond) // revision 含毫秒，避免同毫秒
		resp, err := c.SaveCanvas(ctx, &canvas.SaveCanvasReq{UserId: 7001, Id: doc.Id, Snapshot: snap, IfMatchRevision: cur})
		if err != nil || resp.Conflict {
			t.Fatal(resp, err)
		}
		e := sink.next(t)
		if e.Source != "user" || e.Revision != resp.Revision {
			t.Fatalf("保存事件: %+v", e)
		}
		cur = resp.Revision
		revs = append(revs, cur)
	}
	// 智能体（服务间入口）写入
	out, err := c.AgentCall(ctx, &canvas.AgentCallReq{UserId: 7001, Tool: "canvas_add_nodes", Client: "svc", Agent: "svc:java-1",
		Arguments: `{"canvas_id":"` + doc.Id + `","nodes":[{"type":"text","text":"from agent","ref":"r1"}]}`})
	if err != nil || out.IsError {
		t.Fatal(out, err)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(out.Result), &res)
	e := sink.next(t)
	if e.Source != "agent" || e.Actor != "svc:java-1" || e.Revision != res["revision"] || len(e.ChangedNodeIDs) != 1 {
		t.Fatalf("智能体事件: %+v res=%v", e, res)
	}
	if ids, _ := res["changed_node_ids"].([]any); len(ids) != 1 || ids[0] != e.ChangedNodeIDs[0] {
		t.Fatalf("工具出参应带 changed_node_ids: %v", res)
	}
	revs = append(revs, e.Revision)

	n, err := c.s.Repo.Revision.Count(ctx, doc.Id)
	if err != nil || n != 3 {
		t.Fatalf("应只留最近 3 个: %d %v", n, err)
	}
	// 被裁掉的老版本 → 明确 reason
	_, err = c.GetCanvas(ctx, &canvas.CanvasIdReq{UserId: 7001, Id: doc.Id, Revision: revs[1]})
	if code, _, _ := xerr.Parse(err); code != xerr.NotFound || xerr.ReasonOf(err) != ReasonRevisionNotRetained {
		t.Fatalf("超出保留范围应 4001 + %s: %v", ReasonRevisionNotRetained, err)
	}
	// 保留中的历史版本：快照是当时的内容
	old, err := c.GetCanvas(ctx, &canvas.CanvasIdReq{UserId: 7001, Id: doc.Id, Revision: revs[3]})
	if err != nil || old.Revision != revs[3] || !regexp.MustCompile(`"text":"v2"`).MatchString(old.Snapshot) {
		t.Fatalf("历史版本: %+v %v", old, err)
	}
	// 历史快照不越权：别人拿同一个 revision 也是 4001
	if _, err := c.GetCanvas(ctx, &canvas.CanvasIdReq{UserId: 7002, Id: doc.Id, Revision: revs[3]}); err == nil {
		t.Fatal("他人不应读到历史")
	}
	// 当前 revision 走正常路径
	now, err := c.GetCanvas(ctx, &canvas.CanvasIdReq{UserId: 7001, Id: doc.Id, Revision: revs[len(revs)-1]})
	if err != nil || now.Revision != revs[len(revs)-1] {
		t.Fatal(now, err)
	}
	r, err := c.GetCanvasRevision(ctx, &canvas.CanvasIdReq{UserId: 7001, Id: doc.Id})
	if err != nil || r.Revision != revs[len(revs)-1] || r.NodeCount != 2 {
		t.Fatalf("GetCanvasRevision: %+v %v", r, err)
	}
	if _, err := c.GetCanvasRevision(ctx, &canvas.CanvasIdReq{UserId: 7002, Id: doc.Id}); err == nil {
		t.Fatal("他人取 revision 应 4001")
	}
	// 新建 / 另存也记历史（另存的新画布第一版即可作 base）
	cp, err := c.SaveAsCanvas(ctx, &canvas.SaveAsCanvasReq{UserId: 7001, SrcId: doc.Id})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := c.s.Repo.Revision.Count(ctx, cp.Id); n != 1 {
		t.Fatalf("另存应记第一版: %d", n)
	}
}

// actor 只有服务间入口（client=svc）才认 svc: 前缀；MCP / Function Call 自报的 svc: 一律转义。
func TestAgentActorNoSpoof(t *testing.T) {
	for _, c := range []struct{ client, agent, want string }{
		{"svc", "svc:java-1", "svc:java-1"},
		{"mcp", "svc:java-1", "mcp:svc_java-1"},
		{"function", "SVC:java-1", "function:svc_java-1"},
		{"grpc", " svc:x", "grpc:svc_x"},
		{"mcp", "cursor", "mcp:cursor"},
		{"mcp", "", "mcp"},
	} {
		if got := agentActor(c.client, c.agent); got != c.want {
			t.Fatalf("%s/%s → %s, want %s", c.client, c.agent, got, c.want)
		}
	}
}
