package logic

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/model"
)

func testEditor(t *testing.T, snap string) (*core, *docEditor) {
	t.Helper()
	c := testCore()
	d, err := ParseSnapshot([]byte(snap), 0)
	if err != nil {
		t.Fatal(err)
	}
	return c, newDocEditor(d, c.agentFindModel, c.agentModels, time.UnixMilli(1_700_000_000_000))
}

// applyTool 按工具 Schema 解析入参后在编辑器上执行（与 AgentCall 同一条路径）。
func applyTool(t *testing.T, e *docEditor, tool, args string) (map[string]any, error) {
	t.Helper()
	tl, ok := agenttool.Find(tool)
	if !ok {
		t.Fatalf("没有工具 %s", tool)
	}
	m, err := agenttool.ParseArguments(tl, json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	return applyEditTool(e, tool, m)
}

const cv = `"canvas_id":"cv_0123456789abcdef"`

func mustValid(t *testing.T, c *core, e *docEditor) {
	t.Helper()
	if err := ValidateDoc(e.d, c.s.Config.Limits.MaxNodes, c.imageLimit); err != nil {
		t.Fatalf("编辑结果没通过 ValidateDoc: %v\n%s", err, e.d.Marshal())
	}
}

func TestAgentAddNodesDefaults(t *testing.T) {
	c, e := testEditor(t, EmptySnapshot)
	out, err := applyTool(t, e, agenttool.ToolAddNodes, `{`+cv+`,"nodes":[
		{"ref":"p","type":"text","text":"一只猫"},
		{"ref":"m","type":"image_model","model":"Banana","prompt":"可爱","ratio":"3:4","count":2},
		{"ref":"v","type":"video_model"},
		{"type":"tool","tool":"upscale","x":10,"y":20},
		{"type":"group","title":"分组A"},
		{"type":"image","image_url":"https://cdn.example.com/a.png"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	nodes := out["nodes"].([]*agentNodeOut)
	if len(nodes) != 6 || !strings.HasPrefix(nodes[0].NodeID, "t_") || !strings.HasPrefix(nodes[1].NodeID, "m_") ||
		!strings.HasPrefix(nodes[3].NodeID, "tool_") || !strings.HasPrefix(nodes[4].NodeID, "mod_") || !strings.HasPrefix(nodes[5].NodeID, "n_") {
		t.Fatalf("id 前缀不对: %+v", nodes)
	}
	by := e.d.NodeByID()
	m := by[nodes[1].NodeID]
	if m.Type != "model" || m.Kind() != "model" || strData(m.Data, "modelId") != "banana-v1" || strData(m.Data, "ratio") != "3:4" ||
		intData(m.Data, "count") != 2 || strData(m.Data, "prompt") != "可爱" || !strings.Contains(strData(m.Data, "subtitle"), "Nano Banana") {
		t.Fatalf("图片模型节点 data 不对: %v", m.Data)
	}
	v := by[nodes[2].NodeID]
	if v.Kind() != "video" || strData(v.Data, "model") == "" || strData(v.Data, "duration") != "4s" || v.Data["sound"] != true {
		t.Fatalf("视频模型节点没取默认模型 / 默认参数: %v", v.Data)
	}
	tool := by[nodes[3].NodeID]
	if tool.Tool() != "upscale" || intData(tool.Data, "scale") != 2 || strData(tool.Data, "title") != "高清放大" || nodePos(tool) != (pos{10, 20}) {
		t.Fatalf("工具节点不对: %v %v", tool.Data, nodePos(tool))
	}
	if by[nodes[4].NodeID].raw["zIndex"] == nil || strData(by[nodes[4].NodeID].Data, "title") != "分组A" {
		t.Fatal("分组框应 zIndex=-1 且带标题")
	}
	// 自动放置的节点互不重叠（分组框除外）
	for i, a := range e.d.Nodes {
		for _, b := range e.d.Nodes[i+1:] {
			if a.Type != "module" && b.Type != "module" && nodeRect(a).overlaps(nodeRect(b), 0) {
				t.Fatalf("节点 %s 与 %s 重叠", a.ID, b.ID)
			}
		}
	}
	if e.refs["p"] != nodes[0].NodeID {
		t.Fatal("ref 没登记")
	}
	mustValid(t, c, e)
}

func TestAgentAddNodesRejects(t *testing.T) {
	_, e := testEditor(t, EmptySnapshot)
	cases := map[string]string{
		`{"type":"image_model","model":"不存在的模型"}`:                 "没有这个图片模型",
		`{"type":"image_model","model":"SD5P","size":"1K"}`:       "不在该模型可选范围",
		`{"type":"image_model","count":8}`:                        "最多 4 张",
		`{"type":"text","prompt":"x"}`:                            "只能用于 image_model",
		`{"type":"tool"}`:                                         "必须给 tool",
		`{"type":"image","image_url":"data:image/png;base64,xx"}`: "必须是 http(s)",
		`{"type":"video_model","count":2}`:                        "视频一次只能生成 1 个",
		`{"type":"text","image_url":"https://x/1.png"}`:           "只能用于图片节点",
	}
	for spec, want := range cases {
		_, err := applyTool(t, e, agenttool.ToolAddNodes, `{`+cv+`,"nodes":[`+spec+`]}`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: 想要 %q，得到 %v", spec, want, err)
		}
	}
}

const snapTM = `{"nodes":[
 {"id":"t1","type":"text","position":{"x":0,"y":0},"data":{"kind":"text","text":"猫"}},
 {"id":"t2","type":"text","position":{"x":0,"y":300},"data":{"kind":"text","text":"狗"}},
 {"id":"i1","type":"image","position":{"x":0,"y":600},"data":{"kind":"input","image":"https://cdn.example.com/a.png","assetId":"9"}},
 {"id":"m1","type":"model","position":{"x":500,"y":0},"data":{"kind":"model","model":"Banana","modelId":"banana-v1","ratio":"1:1","size":"1K","count":1,"prompt":"高清"}},
 {"id":"m2","type":"model","position":{"x":900,"y":0},"data":{"kind":"model","model":"Banana","prompt":"续写"}},
 {"id":"x1","type":"tool","position":{"x":900,"y":600},"data":{"kind":"tool","tool":"upscale"}}
],"edges":[],"viewport":{"x":1,"y":2,"zoom":1}}`

func TestAgentConnectInferAndReplace(t *testing.T) {
	c, e := testEditor(t, snapTM)
	out, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[
		{"source":"t1","target":"m1"},{"source":"i1","target":"m1"},{"source":"m1","target":"m2"},{"source":"m1","target":"x1"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	es := out["edges"].([]*agentEdgeOut)
	want := [][3]string{{"text", "prompt", ""}, {"image", "image", "1"}, {"image-out", "image", "1"}, {"image-out", "image", "1"}}
	for i, w := range want {
		if es[i].SourceHandle != w[0] || es[i].TargetHandle != w[1] {
			t.Fatalf("第 %d 条推断错: %+v", i, es[i])
		}
	}
	if es[0].Order != 0 || es[1].Order != 1 {
		t.Fatalf("prompt 不计序、image 从 1 起: %+v %+v", es[0], es[1])
	}
	mustValid(t, c, e)
	// 提示词单槽：t2 → m1 替换 t1 → m1
	out, err = applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"t2","target":"m1"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if rp := out["replaced"].([]string); len(rp) != 1 || rp[0] != es[0].EdgeID {
		t.Fatalf("应替换旧提示词线: %v", out["replaced"])
	}
	// 重复连同一条：幂等，不新增
	before := len(e.d.Edges)
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"i1","target":"m1"}]}`); err != nil || len(e.d.Edges) != before {
		t.Fatalf("重复连线应幂等: %v %d→%d", err, before, len(e.d.Edges))
	}
	// 规则：文字不能进 image 口、不能连自己、成环由 ValidateDoc 拦
	for spec, want := range map[string]string{
		`{"source":"t1","target":"m1","target_handle":"image"}`: "参考图端口只收图片或模型输出",
		`{"source":"m1","target":"m1"}`:                         "不能连到自己",
		`{"source":"m1","target":"t1"}`:                         "没有输入口",
		`{"source":"nope","target":"m1"}`:                       "不存在",
	} {
		if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[`+spec+`]}`); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: 想要 %q，得到 %v", spec, want, err)
		}
	}
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"m2","target":"m1"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDoc(e.d, 0, c.imageLimit); err == nil || !strings.Contains(err.Error(), "环") {
		t.Fatalf("成环应被 ValidateDoc 拦下: %v", err)
	}
	// 顶层未知键（viewport）原样保留
	if !strings.Contains(string(e.d.Marshal()), `"viewport"`) {
		t.Fatal("viewport 丢了")
	}
}

func TestAgentDeleteMoveResizeDisconnect(t *testing.T) {
	c, e := testEditor(t, snapTM)
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"t1","target":"m1"},{"source":"i1","target":"m1"},{"source":"m1","target":"x1"}]}`); err != nil {
		t.Fatal(err)
	}
	e.d.NodeByID()["x1"].Data["sourceNodeId"] = "m1"
	out, err := applyTool(t, e, agenttool.ToolDeleteNodes, `{`+cv+`,"node_ids":["m1","gone"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(out["deleted_nodes"].([]string)) != 1 || len(out["deleted_edges"].([]string)) != 3 || len(e.d.Edges) != 0 {
		t.Fatalf("删节点应连带删线、不存在的忽略: %v", out)
	}
	if strData(e.d.NodeByID()["x1"].Data, "sourceNodeId") != "" {
		t.Fatal("指向被删节点的 sourceNodeId 应清空")
	}
	out, err = applyTool(t, e, agenttool.ToolMoveNodes, `{`+cv+`,"moves":[{"node_id":"t1","x":100,"y":50},{"node_id":"t2","dx":10,"dy":-20}]}`)
	if err != nil {
		t.Fatal(err)
	}
	ps := out["nodes"].([]*agentPosOut)
	if ps[0].X != 100 || ps[0].Y != 50 || ps[1].X != 10 || ps[1].Y != 280 {
		t.Fatalf("移动不对: %+v %+v", ps[0], ps[1])
	}
	out, err = applyTool(t, e, agenttool.ToolResizeNodes, `{`+cv+`,"sizes":[{"node_id":"t1","scale":2},{"node_id":"i1","width":300}]}`)
	if err != nil {
		t.Fatal(err)
	}
	ss := out["nodes"].([]*agentSizeOut)
	if ss[0].Width != 640 || ss[0].Height != 440 || ss[1].Width != 300 || ss[1].Height != 260 {
		t.Fatalf("缩放不对: %+v %+v", ss[0], ss[1])
	}
	if _, err := applyTool(t, e, agenttool.ToolMoveNodes, `{`+cv+`,"moves":[{"node_id":"t1"}]}`); err == nil {
		t.Fatal("没有坐标的移动应报错")
	}
	// 断线：按端点 / 按 id，找不到不报错
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"t1","target":"m2"},{"source":"i1","target":"m2"}]}`); err != nil {
		t.Fatal(err)
	}
	id := e.d.Edges[1].ID
	out, err = applyTool(t, e, agenttool.ToolDisconnect, `{`+cv+`,"pairs":[{"source":"t1","target":"m2"}],"edge_ids":["`+id+`","nope"]}`)
	if err != nil || len(out["removed"].([]string)) != 2 || len(e.d.Edges) != 0 {
		t.Fatalf("断线不对: %v %v", out, err)
	}
	mustValid(t, c, e)
}

func TestAgentAutoLayout(t *testing.T) {
	c, e := testEditor(t, snapTM)
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"t1","target":"m1"},{"source":"m1","target":"m2"},{"source":"m2","target":"x1"}]}`); err != nil {
		t.Fatal(err)
	}
	out, err := applyTool(t, e, agenttool.ToolAutoLayout, `{`+cv+`,"gap_x":100,"origin_x":0,"origin_y":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(out["nodes"].([]agentPosOut)) != 6 {
		t.Fatalf("应排 6 个节点: %v", out)
	}
	by := e.d.NodeByID()
	x := func(id string) float64 { return nodePos(by[id]).X }
	if !(x("t1") < x("m1") && x("m1") < x("m2") && x("m2") < x("x1")) {
		t.Fatalf("LR 分层顺序不对: t1=%v m1=%v m2=%v x1=%v", x("t1"), x("m1"), x("m2"), x("x1"))
	}
	if x("t1") != 0 || x("m1") != 420 { // 第 0 层最宽是文字 320 + 间距 100
		t.Fatalf("层间距不对: t1=%v m1=%v", x("t1"), x("m1"))
	}
	if nodePos(by["t2"]).Y <= nodePos(by["t1"]).Y {
		t.Fatal("同层应按原 y 顺序排列")
	}
	for i, a := range e.d.Nodes {
		for _, b := range e.d.Nodes[i+1:] {
			if nodeRect(a).overlaps(nodeRect(b), 0) {
				t.Fatalf("排版后 %s 与 %s 重叠", a.ID, b.ID)
			}
		}
	}
	// TB + 子集
	if _, err := applyTool(t, e, agenttool.ToolAutoLayout, `{`+cv+`,"direction":"TB","node_ids":["t1","m1"]}`); err != nil {
		t.Fatal(err)
	}
	if nodePos(by["m1"]).Y <= nodePos(by["t1"]).Y {
		t.Fatal("TB 下游应在下方")
	}
	mustValid(t, c, e)
}

func TestAgentBatchEditAtomicAndRefs(t *testing.T) {
	c, e := testEditor(t, EmptySnapshot)
	out, err := applyTool(t, e, agenttool.ToolBatchEdit, `{`+cv+`,"ops":[
		{"op":"add_node","args":{"ref":"p","type":"text","text":"海边日落"}},
		{"op":"add_node","args":{"ref":"m","type":"image_model","model":"Image"}},
		{"op":"connect","args":{"source":"p","target":"m"}},
		{"op":"update_node","args":{"node_id":"m","size":"2K","data":{"cfg":0.8,"status":"done"}}},
		{"op":"auto_layout","args":{}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	refs := out["refs"].(map[string]string)
	m := e.d.NodeByID()[refs["m"]]
	if len(e.d.Edges) != 1 || e.d.Edges[0].Source != refs["p"] || strData(m.Data, "size") != "2K" {
		t.Fatalf("批量结果不对: %s", e.d.Marshal())
	}
	if m.Data["status"] != nil {
		t.Fatal("运行态键不应被智能体写入")
	}
	mustValid(t, c, e)
	// 任一步失败整体报错（调用方不保存）
	_, e2 := testEditor(t, EmptySnapshot)
	_, err = applyTool(t, e2, agenttool.ToolBatchEdit, `{`+cv+`,"ops":[
		{"op":"add_node","args":{"type":"text"}},
		{"op":"connect","args":{"source":"x","target":"y"}}]}`)
	if err == nil || !strings.Contains(err.Error(), "ops[1]") {
		t.Fatalf("第二步失败应报错并带路径: %v", err)
	}
	_, err = applyTool(t, e2, agenttool.ToolBatchEdit, `{`+cv+`,"ops":[{"op":"add_node","args":{"type":"text","bogus":1}}]}`)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("op args 也要按 Schema 校验: %v", err)
	}
}

func TestAgentRunRequestFromGraph(t *testing.T) {
	_, e := testEditor(t, snapTM)
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"t1","target":"m2"},{"source":"i1","target":"m2"},{"source":"m1","target":"m2"}]}`); err != nil {
		t.Fatal(err)
	}
	by := e.d.NodeByID()
	done := map[string]*model.NodeRun{"m1": {NodeID: "m1", SelectedIndex: 1, Status: model.RunSucceeded}}
	req, err := buildAgentRunRequest(e.d, by["m2"], done, map[string]any{"ratio": "16:9", "count": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if req.Kind != "image" || req.Model != "Banana" || req.Prompt != "续写" || req.Ratio != "16:9" || req.Count != 3 {
		t.Fatalf("请求字段不对: %+v", req)
	}
	if len(req.References) != 3 {
		t.Fatalf("应有 3 条引用: %+v", req.References)
	}
	p, i1, m1 := req.References[0], req.References[1], req.References[2]
	if p.Handle != "prompt" || p.Text != "猫" || i1.Image != "https://cdn.example.com/a.png" || i1.AssetId != "9" || i1.Index != 1 ||
		m1.NodeId != "m1" || m1.CandidateIndex == nil || *m1.CandidateIndex != 1 || m1.Index != 2 {
		t.Fatalf("引用不对: %+v %+v %+v", p, i1, m1)
	}
	// 上游没有结果
	if _, err := buildAgentRunRequest(e.d, by["m2"], map[string]*model.NodeRun{}, nil); err == nil || !strings.Contains(err.Error(), "还没有生成结果") {
		t.Fatalf("上游没结果应报错: %v", err)
	}
	// 反推工具作提示词来源：取它最近一次成功的文本
	e.d.Nodes = append(e.d.Nodes, &Node{ID: "i2p", Type: "tool", Data: map[string]any{"kind": "tool", "tool": "i2p"}})
	e.nodes["i2p"] = e.d.Nodes[len(e.d.Nodes)-1]
	if _, err := applyTool(t, e, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"i2p","target":"m1"}]}`); err != nil {
		t.Fatal(err)
	}
	req, err = buildAgentRunRequest(e.d, by["m1"], map[string]*model.NodeRun{"i2p": {ResultText: sql.NullString{String: "反推文本", Valid: true}}}, nil)
	if err != nil || req.References[0].Text != "反推文本" {
		t.Fatalf("反推提示词: %+v %v", req, err)
	}
	// 没选模型
	delete(by["m2"].Data, "model")
	if _, err := buildAgentRunRequest(e.d, by["m2"], done, nil); err == nil || !strings.Contains(err.Error(), "还没选模型") {
		t.Fatalf("没模型应报错: %v", err)
	}
}

func TestAgentRunIDDerivation(t *testing.T) {
	a, b := agentRunID(7, "", "call-1", 0), agentRunID(7, "", "call-1", 0)
	if a != b || !strings.HasPrefix(a, "ag_") || len(a) > 64 {
		t.Fatalf("同 call_id 应派生同 run_id: %s %s", a, b)
	}
	if agentRunID(8, "", "call-1", 0) == a {
		t.Fatal("不同用户不应撞 run_id")
	}
	if agentRunID(7, "mine", "call-1", 0) != "mine" {
		t.Fatal("显式 run_id 优先")
	}
	if agentRunID(7, "", "", 0) == agentRunID(7, "", "", 0) {
		t.Fatal("无 call_id 时随机")
	}
}
