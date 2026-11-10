package logic

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
)

func TestNodeCountAndCover(t *testing.T) {
	d := mustDoc(t, `{"nodes":[{"id":"g","type":"module","data":{}},{"id":"t","type":"text","data":{"text":"x"}},
	 {"id":"m","type":"model","data":{"candidates":["https://c/1.png"]}},{"id":"i","type":"image","data":{"image":"https://c/i.png"}}],"edges":[],"viewport":{"x":1}}`)
	if d.NodeCount() != 4 {
		t.Fatalf("node_count=%d", d.NodeCount())
	}
	if d.Cover() != "https://c/1.png" {
		t.Fatalf("cover=%s", d.Cover())
	}
	if !strings.Contains(string(d.Marshal()), `"viewport":{"x":1}`) {
		t.Fatal("未知顶层键应保留")
	}
}

func TestStripRuntime(t *testing.T) {
	d := mustDoc(t, `{"nodes":[
	 {"id":"i","type":"image","position":{"x":1,"y":2},"zIndex":3,"data":{"kind":"input","image":"https://u/a.png","status":"done","assetId":"9"}},
	 {"id":"m","type":"model","data":{"model":"Banana","status":"done","progress":100,"runId":"r1","candidates":["https://c/1.png"],"image":"https://c/1.png","count":2}},
	 {"id":"m2","type":"model","data":{"model":"Banana","status":"done","image":"https://demo/x.png"}}],"edges":[]}`)
	d.StripRuntime(map[string]bool{"m": true})
	by := d.NodeByID()
	if by["i"].Data["image"] != "https://u/a.png" || by["i"].Data["assetId"] != "9" {
		t.Fatal("输入图节点的 image / assetId 是用户内容，必须保留")
	}
	if _, ok := by["i"].Data["status"]; ok {
		t.Fatal("status 应剥离")
	}
	for _, k := range []string{"status", "progress", "runId", "candidates", "image"} {
		if _, ok := by["m"].Data[k]; ok {
			t.Fatalf("有 run 的模型节点应剥离 %s", k)
		}
	}
	if by["m"].Data["count"] == nil {
		t.Fatal("参数字段应保留")
	}
	if by["m2"].Data["image"] != "https://demo/x.png" {
		t.Fatal("没有 run 的模型节点保留成品（防丢内容）")
	}
	out := string(d.Marshal())
	if !strings.Contains(out, `"zIndex":3`) || !strings.Contains(out, `"position":{"x":1,"y":2}`) {
		t.Fatalf("节点外层字段应原样保留: %s", out)
	}
}

func TestBackfill(t *testing.T) {
	d := mustDoc(t, `{"nodes":[{"id":"m","type":"model","data":{"model":"Banana"}},{"id":"v","type":"model","data":{"kind":"video"}},
	 {"id":"p","type":"tool","data":{"tool":"i2p"}},{"id":"f","type":"model","data":{}}],"edges":[]}`)
	cands, _ := json.Marshal([]model.Candidate{{URL: "https://c/1.png", W: 64, H: 64}, {URL: "https://c/2.png"}})
	runs := []*model.NodeRun{
		{ID: 5, NodeID: "m", RunID: "r-m", Kind: model.KindImage, Status: model.RunSucceeded, Candidates: cands, SelectedIndex: 1, RequestedCount: 2},
		{ID: 6, NodeID: "v", RunID: "r-v", Kind: model.KindVideo, Status: model.RunSucceeded, Candidates: cands, VideoURL: sql.NullString{String: "https://c/v.mp4", Valid: true}},
		{ID: 7, NodeID: "p", RunID: "r-p", Kind: model.KindToolI2P, Status: model.RunSucceeded, ResultText: sql.NullString{String: "一只猫", Valid: true}},
		{ID: 8, NodeID: "f", RunID: "r-f", Kind: model.KindImage, Status: model.RunFailed, ErrorReason: sql.NullString{String: "boom", Valid: true}},
	}
	var ok []*model.NodeRun
	for _, r := range runs {
		if r.Status == model.RunSucceeded {
			ok = append(ok, r)
		}
	}
	d.Backfill(runs, ok, func(r *model.NodeRun) int32 { return 100 })
	by := d.NodeByID()
	m := by["m"].Data
	if m["status"] != "done" || m["runId"] != "r-m" || m["image"] != "https://c/2.png" || m["selectedCandidate"] != 1 {
		t.Fatalf("model 回填不对: %+v", m)
	}
	if c, _ := m["candidates"].([]string); len(c) != 2 {
		t.Fatalf("candidates 应为 URL 数组: %+v", m["candidates"])
	}
	if by["v"].Data["video"] != "https://c/v.mp4" {
		t.Fatal("视频回填")
	}
	if by["p"].Data["resultText"] != "一只猫" {
		t.Fatal("反推回填")
	}
	if by["f"].Data["status"] != "error" || by["f"].Data["errorMessage"] != "boom" {
		t.Fatal("失败回填")
	}
}

func TestRemapIDs(t *testing.T) {
	d := mustDoc(t, `{"nodes":[
	 {"id":"tpl-n1","type":"module","data":{}},
	 {"id":"tpl-n2","type":"text","data":{"mentions":[{"id":"tpl-n3"},{"id":"outside"}]}},
	 {"id":"tpl-n3","type":"image","parentNode":"tpl-n1","data":{"kind":"input"}},
	 {"id":"tpl-n4","type":"tool","data":{"tool":"crop","sourceNodeId":"tpl-n3"}},
	 {"id":"tpl-n5","type":"model","data":{}}],
	 "edges":[{"id":"tpl-e1","source":"tpl-n2","target":"tpl-n5","sourceHandle":"text","targetHandle":"prompt"},
	          {"id":"tpl-e2","source":"tpl-n3","target":"tpl-n5","sourceHandle":"image","targetHandle":"image","data":{"order":1}}]}`)
	n := 0
	m := d.RemapIDs(func(p string) string { n++; return p + "_" + strings.Repeat("x", n) })
	by := d.NodeByID()
	for old, nid := range m {
		if old == nid || by[nid] == nil {
			t.Fatalf("%s → %s 未重映射", old, nid)
		}
	}
	if d.Edges[0].Source != m["tpl-n2"] || d.Edges[0].Target != m["tpl-n5"] || d.Edges[1].Source != m["tpl-n3"] {
		t.Fatal("内部连线应跟着改")
	}
	if strings.HasPrefix(d.Edges[0].ID, "tpl-") || d.Edges[0].ID == d.Edges[1].ID {
		t.Fatal("连线 id 应重新生成")
	}
	if by[m["tpl-n4"]].Data["sourceNodeId"] != m["tpl-n3"] {
		t.Fatal("sourceNodeId 应跟着改")
	}
	ms := by[m["tpl-n2"]].Data["mentions"].([]any)
	if ms[0].(map[string]any)["id"] != m["tpl-n3"] || ms[1].(map[string]any)["id"] != "outside" {
		t.Fatal("mentions 内部引用改、外部引用不动")
	}
	out := string(d.Marshal())
	if !strings.Contains(out, `"parentNode":"`+m["tpl-n1"]+`"`) || strings.Contains(out, "tpl-n") {
		t.Fatalf("重映射后不应残留旧 id: %s", out)
	}
	if err := ValidateDoc(d, 0, nil); err != nil {
		t.Fatalf("重映射后的快照应仍合法: %v", err)
	}
}

func TestRevisionShape(t *testing.T) {
	r := NewRevision(time.UnixMilli(1761800000123))
	if !strings.HasPrefix(r, "1761800000123-") || len(r) != len("1761800000123-abcd") {
		t.Fatalf("revision=%s", r)
	}
	if NewRevision(time.Now()) == NewRevision(time.Now()) && NewRevision(time.Now()) == NewRevision(time.Now()) {
		t.Fatal("revision 应带随机段")
	}
	if id := NewCanvasID(); !strings.HasPrefix(id, "cv_") || len(id) != 19 {
		t.Fatalf("canvas id=%s", id)
	}
}

// 用户侧模板详情：快照只带结构（model / tool 成品与运行态剥掉，输入节点内容与连线保留）。
func TestStructureOnlyStripsResults(t *testing.T) {
	in := &canvas.TemplateInfo{Id: 1, Snapshot: `{"nodes":[` +
		`{"id":"a","type":"image","position":{"x":0,"y":0},"data":{"image":"https://x/in.png","status":"done"}},` +
		`{"id":"b","type":"model","position":{"x":300,"y":0},"data":{"prompt":"p","image":"https://x/out.png","candidates":["c"],"runId":"r1","status":"done"}},` +
		`{"id":"v","type":"model","position":{"x":0,"y":300},"data":{"inputOnly":true,"video":"https://x/ref.mp4","image":"https://x/ref.jpg"}}],` +
		`"edges":[{"id":"e1","source":"a","target":"b"}]}`}
	out := structureOnly(in)
	d, err := ParseSnapshot([]byte(out.Snapshot), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Nodes) != 3 || len(d.Edges) != 1 {
		t.Fatalf("结构丢了: %s", out.Snapshot)
	}
	a, b := d.Nodes[0].Data, d.Nodes[1].Data
	if a["image"] == nil || a["status"] != nil {
		t.Fatalf("输入节点应保留内容、去状态: %v", a)
	}
	if b["image"] != nil || b["candidates"] != nil || b["runId"] != nil || b["status"] != nil || b["prompt"] == nil {
		t.Fatalf("模型节点应只留参数: %v", b)
	}
	if v := d.Nodes[2].Data; v["video"] == nil || v["image"] == nil {
		t.Fatalf("参考素材节点（inputOnly）应保留素材: %v", v)
	}
	if structureOnly(&canvas.TemplateInfo{Snapshot: "not json"}).Snapshot != "not json" {
		t.Fatal("解析失败应原样返回")
	}
}
