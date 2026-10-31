package logic

import (
	"strings"
	"testing"

	"aigc-local-life/pkg/xerr"
)

func mustDoc(t *testing.T, s string) *Doc {
	t.Helper()
	d, err := ParseSnapshot([]byte(s), 0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// 基础节点：文字 t1/t2、输入图 i1..i4、图片模型 m1（Banana, maxImageRefs 2）、视频模型 v1、参考视频模型 v2、
// 反推 p1、裁剪 c1、试穿 y1、结果图 r1、模块框 g1。
const baseNodes = `[
 {"id":"t1","type":"text","data":{"kind":"text","text":"a"}},
 {"id":"t2","type":"text","data":{"kind":"text","text":"b"}},
 {"id":"i1","type":"image","data":{"kind":"input","image":"https://x/1.png"}},
 {"id":"i2","type":"image","data":{"kind":"input","image":"https://x/2.png"}},
 {"id":"i3","type":"image","data":{"kind":"input","image":"https://x/3.png"}},
 {"id":"vi","type":"video","data":{"kind":"video-input","video":"https://x/v.mp4"}},
 {"id":"m1","type":"model","data":{"kind":"model","model":"Banana","maxImageRefs":2}},
 {"id":"m2","type":"model","data":{"kind":"model","model":"Image"}},
 {"id":"v1","type":"model","data":{"kind":"video","model":"Seedance 2.0"}},
 {"id":"v2","type":"model","data":{"kind":"video","model":"Seedance 2.0","inputOnly":true}},
 {"id":"p1","type":"tool","data":{"kind":"tool","tool":"i2p"}},
 {"id":"c1","type":"tool","data":{"kind":"tool","tool":"crop","sourceNodeId":"i1"}},
 {"id":"y1","type":"tool","data":{"kind":"tool","tool":"tryon"}},
 {"id":"r1","type":"image","data":{"kind":"result"}},
 {"id":"g1","type":"module","data":{"kind":"module"}}
]`

func doc(t *testing.T, edges string) *Doc {
	return mustDoc(t, `{"nodes":`+baseNodes+`,"edges":`+edges+`}`)
}

func e(id, s, sh, tg, th string) string {
	return `{"id":"` + id + `","source":"` + s + `","sourceHandle":"` + sh + `","target":"` + tg + `","targetHandle":"` + th + `"}`
}

func edges(es ...string) string { return "[" + strings.Join(es, ",") + "]" }

func wantParam(t *testing.T, err error, sub string) {
	t.Helper()
	code, msg, ok := xerr.Parse(err)
	if !ok || code != xerr.ParamError || !strings.Contains(msg, sub) {
		t.Fatalf("want 1003 containing %q, got %v", sub, err)
	}
}

func TestValidateDocOK(t *testing.T) {
	d := doc(t, edges(
		e("e1", "t1", "text", "m1", "prompt"),
		e("e2", "i1", "image", "m1", "image"),
		e("e3", "c1", "image-out", "m1", "image"),
		e("e4", "p1", "text", "m2", "prompt"),
		e("e5", "m1", "candidate-1", "m2", "image"),
		e("e6", "v2", "video", "v1", "video"),
		e("e7", "i2", "image", "v1", "image1"),
		e("e8", "i3", "image", "v1", "image2"),
		e("e9", "i1", "image", "y1", "garment"),
		e("e10", "i2", "image", "y1", "models"),
		e("e11", "i3", "image", "y1", "models"),
		e("e12", "m2", "image-out", "r1", "image-in"),
		e("e13", "vi", "video", "m2", "image"),
	))
	if err := ValidateDoc(d, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDocRules(t *testing.T) {
	cases := []struct {
		name, edges, want string
	}{
		{"提示词只收文字或反推", edges(e("e1", "i1", "image", "m1", "prompt")), "提示词端口只收"},
		{"提示词收裁剪工具被拒", edges(e("e1", "c1", "image-out", "m1", "prompt")), "提示词端口只收"},
		{"提示词最多 1 条", edges(e("e1", "t1", "text", "m1", "prompt"), e("e2", "t2", "text", "m1", "prompt")), "提示词最多接 1 条"},
		{"参考图超 maxImageRefs", edges(e("e1", "i1", "image", "m1", "image"), e("e2", "i2", "image", "m1", "image"), e("e3", "i3", "image", "m1", "image")), "参考图最多接 2 条"},
		{"参考图不收文字", edges(e("e1", "t1", "text", "m1", "image")), "参考图端口只收"},
		{"参考视频只收视频模型", edges(e("e1", "m2", "image-out", "v1", "video")), "参考视频端口只收"},
		{"参考视频 1 条", edges(e("e1", "v2", "video", "v1", "video"), e("e2", "v2", "video-2", "v1", "video")), "参考视频最多接 1 条"},
		{"image1 只能 1 条", edges(e("e1", "i1", "image", "v1", "image1"), e("e2", "i2", "image", "v1", "image1")), "图片 1最多接 1 条"},
		{"首尾帧也占参考图名额", edges(e("e1", "i1", "image", "v1", "image1"), e("e2", "i2", "image", "v1", "image2"), e("e3", "i3", "image", "v1", "image")), "参考图最多接 2 条"},
		{"音频暂不接入", edges(e("e1", "i1", "image", "m1", "audio")), "音频参考暂未接入"},
		{"不能自连", edges(e("e1", "m1", "image-out", "m1", "image")), "不能连到自己"},
		{"目标必须有输入口", edges(e("e1", "t1", "text", "i1", "image")), "没有输入口"},
		{"结果图只有 image-in", edges(e("e1", "m1", "image-out", "r1", "image")), "结果图片只有一个输入口"},
		{"结果图 1 条", edges(e("e1", "m1", "image-out", "r1", "image-in"), e("e2", "m2", "image-out", "r1", "image-in")), "结果图片输入最多接 1 条"},
		{"试穿只有两个口", edges(e("e1", "i1", "image", "y1", "image")), "服装和模特两个输入口"},
		{"试穿服装 1 条", edges(e("e1", "i1", "image", "y1", "garment"), e("e2", "i2", "image", "y1", "garment")), "服装最多接 1 条"},
		{"未知端口", edges(e("e1", "i1", "image", "m1", "foo")), "请连到有编号的输入端口"},
		{"引用不存在的节点", edges(e("e1", "zz", "image", "m1", "image")), "引用的节点不存在"},
		{"重复连线", edges(e("e1", "i1", "image", "m2", "image"), e("e2", "i1", "image", "m2", "image")), "重复"},
		{"成环", edges(e("e1", "m1", "image-out", "m2", "image"), e("e2", "m2", "image-out", "m1", "image")), "形成环"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantParam(t, ValidateDoc(doc(t, c.edges), 0, nil), c.want) })
	}
}

func TestTryonModelsLimit(t *testing.T) {
	nodes := `[{"id":"y","type":"tool","data":{"tool":"tryon"}}`
	var es []string
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		nodes += `,{"id":"` + id + `","type":"image","data":{"kind":"input"}}`
		es = append(es, e("e"+id, id, "image", "y", "models"))
	}
	d := mustDoc(t, `{"nodes":`+nodes+`],"edges":`+edges(es...)+`}`)
	wantParam(t, ValidateDoc(d, 0, nil), "模特最多接 9 条")
	d.Edges = d.Edges[:9]
	if err := ValidateDoc(d, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDocNodes(t *testing.T) {
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"text","data":{}},{"id":"a","type":"text","data":{}}],"edges":[]}`), 0, nil), "节点 id 重复")
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"","type":"text","data":{}}],"edges":[]}`), 0, nil), "缺少 id")
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"weird","data":{}}],"edges":[]}`), 0, nil), "类型不支持")
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"image","data":{"image":"data:image/png;base64,AAAA"}}],"edges":[]}`), 0, nil), "请先上传为地址")
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"tool","data":{"maskDataUrl":"data:image/png;base64,AA"}}],"edges":[]}`), 0, nil), "dataURL")
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"tool","data":{"tryonModels":[{"image":"blob:http://x/1"}]}}],"edges":[]}`), 0, nil), "dataURL")
	// 文字内容里出现 data: 不算
	if err := ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"text","data":{"text":"data: 一段文字"}}],"edges":[]}`), 0, nil); err != nil {
		t.Fatal(err)
	}
	wantParam(t, ValidateDoc(mustDoc(t, `{"nodes":[{"id":"a","type":"text","data":{}},{"id":"b","type":"text","data":{}}],"edges":[]}`), 1, nil), "节点数超过上限")
}

func TestParseSnapshotLimits(t *testing.T) {
	big := `{"nodes":[{"id":"a","type":"text","data":{"text":"` + strings.Repeat("x", 3<<20) + `"}}],"edges":[]}`
	if _, err := ParseSnapshot([]byte(big), 2<<20); err == nil {
		t.Fatal("超过 2MB 应拒绝")
	} else {
		wantParam(t, err, "超过 2MB")
	}
	for _, bad := range []string{`[]`, `{"nodes":{}}`, `{"edges":1}`, `not json`} {
		if _, err := ParseSnapshot([]byte(bad), 0); err == nil {
			t.Fatalf("%s 应拒绝", bad)
		}
	}
	if d, err := ParseSnapshot(nil, 0); err != nil || len(d.Nodes) != 0 {
		t.Fatalf("空快照应视为空画布: %v", err)
	}
}

func TestValidateRun(t *testing.T) {
	d := doc(t, edges(e("e1", "t1", "text", "m1", "prompt"), e("e2", "i1", "image", "m1", "image"), e("e3", "i2", "image", "m1", "image")))
	ok := RunCheck{NodeID: "m1", Kind: "image", ImageLimit: 2, Refs: []RefInput{
		{Handle: "prompt", NodeID: "t1"}, {Handle: "image", NodeID: "i1", URL: "https://x/1.png"}, {Handle: "image", NodeID: "i2", URL: "https://x/2.png"},
	}}
	if err := ValidateRun(d, ok); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		c    RunCheck
		want string
	}{
		{"引用自己", RunCheck{NodeID: "m1", ImageLimit: 2, Refs: []RefInput{{Handle: "image", NodeID: "m1"}}}, "不能引用节点自己"},
		{"dataURL", RunCheck{NodeID: "m1", ImageLimit: 2, Refs: []RefInput{{Handle: "image", NodeID: "i1", URL: "data:image/png;base64,AA"}}}, "dataURL"},
		{"提示词 2 条", RunCheck{NodeID: "m1", ImageLimit: 2, Refs: []RefInput{{Handle: "prompt", NodeID: "t1"}, {Handle: "prompt", NodeID: "t2"}}}, "提示词最多接 1 条"},
		{"参考图超限", RunCheck{NodeID: "m1", ImageLimit: 1, Refs: []RefInput{{Handle: "image", NodeID: "i1"}, {Handle: "image", NodeID: "i2"}}}, "参考图最多接 1 条"},
		{"参考视频 2 条", RunCheck{NodeID: "v9", ImageLimit: 3, Refs: []RefInput{{Handle: "video", NodeID: "v2"}, {Handle: "video", NodeID: "v3"}}}, "参考视频最多接 1 条"},
		{"image1 两条", RunCheck{NodeID: "v9", ImageLimit: 3, Refs: []RefInput{{Handle: "image1", NodeID: "i1"}, {Handle: "image1", NodeID: "i2"}}}, "图片 1 最多接 1 条"},
		{"没连线的引用", RunCheck{NodeID: "m1", ImageLimit: 2, Refs: []RefInput{{Handle: "image", NodeID: "i3"}}}, "没有连线"},
		{"来源类型不对", RunCheck{NodeID: "m1", ImageLimit: 2, Refs: []RefInput{{Handle: "prompt", NodeID: "i1"}}}, "提示词端口只收"},
		{"未知 handle", RunCheck{NodeID: "m1", ImageLimit: 2, Refs: []RefInput{{Handle: "xx", NodeID: "i1"}}}, "handle 不支持"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantParam(t, ValidateRun(d, c.c), c.want) })
	}
	// 工具节点按 sourceNodeId 认上游，不要求连线
	if err := ValidateRun(d, RunCheck{NodeID: "c1", Kind: "tool_crop", ImageLimit: 1, Refs: []RefInput{{Handle: "image", NodeID: "i1"}}}); err != nil {
		t.Fatal(err)
	}
	// 节点还没保存：只做数量校验
	if err := ValidateRun(d, RunCheck{NodeID: "new", ImageLimit: 2, Refs: []RefInput{{Handle: "image", NodeID: "i3"}}}); err != nil {
		t.Fatal(err)
	}
}

// 工具节点（裁剪 / 反推等）的图片输入口：可连一条图片类来源，只接 image 口，每个工具 1 条。
func TestToolInputEdge(t *testing.T) {
	if err := ValidateDoc(doc(t, edges(e("e9", "i2", "image", "c1", "image"))), 0, nil); err != nil {
		t.Fatalf("crop with one image edge should be valid: %v", err)
	}
	if err := ValidateDoc(doc(t, edges(e("e9", "c1", "image-out", "p1", "image"))), 0, nil); err != nil {
		t.Fatalf("tool output into i2p image should be valid: %v", err)
	}
	wantParam(t, ValidateDoc(doc(t, edges(e("e9", "i2", "image", "c1", "image"), e("e10", "i3", "image", "c1", "image"))), 0, nil), "工具输入图")
	wantParam(t, ValidateDoc(doc(t, edges(e("e9", "i2", "image", "c1", "prompt"))), 0, nil), "工具节点只有一个图片输入口")
	wantParam(t, ValidateDoc(doc(t, edges(e("e9", "t1", "text", "c1", "image"))), 0, nil), "参考图端口只收")
}
