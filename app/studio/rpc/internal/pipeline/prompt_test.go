package pipeline

import (
	"strings"
	"testing"
)

const body = "【任务】把图2的上衣穿到图1模特身上。\n\n【锁定】背景、光线保持不变。\n  末尾空格  "

func TestWithImageOrderLine(t *testing.T) {
	cases := []struct {
		kind  string
		n     int
		first string
	}{
		{KindTryon, 5, "图序：图1 是模特，图2–图5 是服装参考图（主图在前，局部细节在后）。"},
		{KindTryon, 2, "图序：图1 是模特，图2 是服装参考图。"},
		{KindPose, 2, "图序：图1 是原图，图2 是姿势参考图。"},
		{KindImageGen, 3, "图序：图1–图3 是参考图。"},
	}
	for _, c := range cases {
		got := WithImageOrderLine(body, c.kind, c.n)
		first, rest, _ := strings.Cut(got, "\n")
		if first != c.first {
			t.Errorf("%s/%d first line = %q", c.kind, c.n, first)
		}
		if rest != body { // E9 / F3：正文逐字一致
			t.Errorf("%s body changed", c.kind)
		}
		if StripImageOrderLine(got) != body {
			t.Errorf("strip failed")
		}
		// 幂等：重复调用不叠行
		if again := WithImageOrderLine(got, c.kind, c.n); again != got {
			t.Errorf("not idempotent: %q", again)
		}
	}
	if WithImageOrderLine(body, KindImageGen, 0) != body {
		t.Fatal("imagegen without refs must be unchanged")
	}
}

func TestBuildTryonAnalyzeReq(t *testing.T) {
	spec := TryonSpec{
		Items: []string{"top"}, Fabric: "上衣：真丝；鞋子：皮革", Notes: "  注意领口  ",
		Lengths: map[string]string{"top": "及腰", "dress": "及膝"}, Sleeves: map[string]string{"bottom": "x"},
	}
	sel := Selection{Images: []Image{
		{Kind: ImageGarment, Item: "top", Role: "上衣", Data: "data:image/jpeg;base64,AA"},
		{Kind: ImageDetail, Item: "top", Label: "上衣·领口", Data: "data:image/jpeg;base64,BB"},
		{Kind: ImageModel, Key: "m-1", Role: RoleModel, Data: "data:image/jpeg;base64,CC"},
	}, Model: true}
	req := BuildTryonAnalyzeReq(spec, sel)
	if req.Category != "top" || req.SetMode != "one" || !req.Deep || req.Notes != "注意领口" {
		t.Fatalf("req = %+v", req)
	}
	if req.Fabric != spec.Fabric { // E4：面料按件原样透传
		t.Fatalf("fabric = %q", req.Fabric)
	}
	if len(req.Lengths) != 1 || req.Lengths["top"] != "及腰" || req.Sleeves != nil || req.LengthClass != "及腰" {
		t.Fatalf("lengths = %v sleeves = %v lc = %q", req.Lengths, req.Sleeves, req.LengthClass)
	}
	if req.Images[0].Role != "上衣" || req.Images[1].Role != RoleDetail || req.Images[1].Label != "上衣·领口" || req.Images[2].Role != RoleModel {
		t.Fatalf("images = %+v", req.Images)
	}

	spec.Items = []string{"top", "bottom"}
	models := []Image{{Key: "m-1", Data: "data:image/jpeg;base64,CC"}, {Key: "m-2", Data: "data:image/jpeg;base64,DD"}}
	breq := BuildTryonAnalyzeBatchReq(spec, sel, models, 3)
	if breq.Category != CategoryCompose || len(breq.Images) != 2 || len(breq.Models) != 2 || breq.Models[1].ID != "m-2" || breq.Concurrency != 3 || breq.LengthClass != "" {
		t.Fatalf("batch = %+v", breq)
	}
	if len(sel.Images) != 3 {
		t.Fatal("batch builder must not mutate selection")
	}
}

func TestBuildPoseNotes(t *testing.T) {
	cases := map[[2]string]string{
		{"全局", "本张"}:  "全局\n本张",
		{"  ", "本张 "}: "本张",
		{"全局", ""}:    "全局",
		{"", ""}:      "",
	}
	for in, want := range cases {
		if got := BuildPoseNotes(in[0], in[1]); got != want {
			t.Errorf("%q = %q want %q", in, got, want)
		}
	}
}
