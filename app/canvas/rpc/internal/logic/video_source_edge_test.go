package logic

import (
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/model"
)

// 视频素材节点（type=video，用户上传）：只能连参考视频口。旧规则只放行图片类口，运行时一定取不到图，
// 智能体因此把视频接到「参考图」、报价报「没有素材」。

func TestVideoSourceOnlyFeedsVideoPort(t *testing.T) {
	if err := ValidateDoc(doc(t, edges(e("e1", "vi", "video", "v1", "video"))), 0, nil); err != nil {
		t.Fatalf("视频素材 → 参考视频口应通过: %v", err)
	}
	// 绕过读快照修正，直接校验：图片类口一律拒收，并说明该连哪
	for _, th := range []string{"image", "image1", "image2"} {
		d := doc(t, "[]")
		d.Edges = append(d.Edges, &Edge{ID: "x", Source: "vi", SourceHandle: "video", Target: "v1", TargetHandle: th})
		wantParam(t, ValidateDoc(d, 0, nil), "视频素材请连到参考视频口")
	}
	d := doc(t, "[]")
	d.Edges = append(d.Edges, &Edge{ID: "x", Source: "vi", SourceHandle: "video", Target: "c1", TargetHandle: "image"})
	wantParam(t, ValidateDoc(d, 0, nil), "视频素材请连到参考视频口")
}

func TestRepairVideoSourceEdgesOnRead(t *testing.T) {
	d := doc(t, edges(
		e("a", "vi", "video", "v1", "image1"), // 视频模型、参考视频口空 → 改连 video
		e("b", "vi", "video", "m2", "image"),  // 图片模型：从未生效 → 去掉
		e("c", "i1", "image", "v1", "image2"), // 正常线不动
	))
	got := map[string]string{}
	for _, x := range d.Edges {
		got[x.ID] = x.TargetHandle
	}
	if len(got) != 2 || got["a"] != "video" || got["c"] != "image2" {
		t.Fatalf("修正结果不对: %v", got)
	}
	if err := ValidateDoc(d, 0, nil); err != nil {
		t.Fatalf("修正后应能通过校验（保存不被旧线卡住）: %v", err)
	}
	// 写回 JSON 带上新口名，下次保存即落库
	var out struct {
		Edges []map[string]any `json:"edges"`
	}
	_ = json.Unmarshal(d.Marshal(), &out)
	for _, x := range out.Edges {
		if x["id"] == "a" && x["targetHandle"] != "video" {
			t.Fatalf("写回的 targetHandle 没更新: %v", x)
		}
	}
	// 参考视频口已被占：旧线去掉，不挤掉已有的参考视频
	d = doc(t, edges(e("a", "v2", "video", "v1", "video"), e("b", "vi", "video", "v1", "image")))
	if len(d.Edges) != 1 || d.Edges[0].ID != "a" {
		t.Fatalf("参考视频口已占用时应去掉旧线: %+v", d.Edges)
	}
}

func TestAgentConnectVideoSourceToVideoPort(t *testing.T) {
	snap := `{"nodes":` + baseNodes + `,"edges":[]}`
	c, ed := testEditor(t, snap)
	out, err := applyTool(t, ed, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"vi","target":"v1"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if es := out["edges"].([]*agentEdgeOut); es[0].SourceHandle != "video" || es[0].TargetHandle != "video" {
		t.Fatalf("视频素材 → 视频模型应自动连参考视频口: %+v", es[0])
	}
	mustValid(t, c, ed)
	_, err = applyTool(t, ed, agenttool.ToolConnect, `{`+cv+`,"edges":[{"source":"vi","target":"v1","target_handle":"image"}]}`)
	if err == nil || !strings.Contains(err.Error(), "视频素材请连到参考视频口") {
		t.Fatalf("显式连参考图应被拒并说明原因: %v", err)
	}
}

func TestAgentReferencesTakeUploadedVideo(t *testing.T) {
	d := doc(t, edges(e("a", "vi", "video", "v1", "video")))
	refs, err := agentReferences(d, d.NodeByID()["v1"], map[string]*model.NodeRun{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Handle != "video" || refs[0].Video != "https://x/v.mp4" {
		t.Fatalf("应带上视频素材的地址: %+v", refs)
	}
	d = mustDoc(t, `{"nodes":[{"id":"vi","type":"video","data":{"kind":"video-input"}},
		{"id":"v1","type":"model","data":{"kind":"video","model":"Seedance 2.0"}}],"edges":[`+e("a", "vi", "video", "v1", "video")+`]}`)
	if _, err := agentReferences(d, d.NodeByID()["v1"], map[string]*model.NodeRun{}); err == nil ||
		!strings.Contains(err.Error(), "还没有上传视频") {
		t.Fatalf("没上传视频应明确报错: %v", err)
	}
}
