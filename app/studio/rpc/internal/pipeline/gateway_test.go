package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"aigc-local-life/pkg/aigateway"
)

var groups = map[string]string{
	"通用": "img.gpt-image-2", "GPT": "img.gpt-image-2", "Banana": "img.nano-banana-2.1", "Seedream": "img.seedream-5-pro",
}

func TestMapModel(t *testing.T) {
	cases := map[string]string{
		"": "img.gpt-image-2", "通用": "img.gpt-image-2", "general": "img.gpt-image-2",
		"GPT": "img.gpt-image-2", "gpt": "img.gpt-image-2", "gpt-image-2": "img.gpt-image-2",
		"Banana": "img.nano-banana-2.1", "nano-banana": "img.nano-banana-2.1", "nano": "img.nano-banana-2.1",
		"Seedream": "img.seedream-5-pro", "seedream-5-pro": "img.seedream-5-pro", " SEEDREAM ": "img.seedream-5-pro",
		"img.seedream-5-pro": "img.seedream-5-pro",
	}
	for in, want := range cases {
		got, err := MapModel(in, groups)
		if err != nil || got != want {
			t.Errorf("%q = %q, %v want %q", in, got, err, want)
		}
	}
	if _, err := MapModel("midjourney", groups); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := MapModel("Banana", map[string]string{"通用": "x"}); err == nil {
		t.Fatal("missing group must error")
	}
}

func TestResolution(t *testing.T) {
	for in, want := range map[string]string{"": "1K", "1k": "1K", "2K": "2K", " 4k ": "4K"} {
		if got, err := GatewayResolution(in); err != nil || got != want {
			t.Errorf("%q = %q %v", in, got, err)
		}
	}
	if r, _ := NormalizeResolution("2K"); r != "2k" {
		t.Fatal("normalize lower")
	}
	if _, err := NormalizeResolution("8k"); err == nil {
		t.Fatal("8k must fail")
	}
}

func refs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("mr://r%d", i+1)
	}
	return out
}

func TestBuildImageEditRequest(t *testing.T) {
	req, warns, err := BuildImageEditRequest(KindTryon, "img.gpt-image-2", body, refs(5), "2k", "3:4", map[string]string{"batch": "12", "task": "tyo1"})
	if err != nil || len(warns) != 0 {
		t.Fatal(err, warns)
	}
	if req.Operation != OpImageEdit || req.ModelGroup != "img.gpt-image-2" || req.MediaPurpose != aigateway.MediaPurposeIntermediate {
		t.Fatalf("req = %+v", req)
	}
	in := req.Input.(ImageEditInput)
	if len(in.Images) != 5 || in.Images[0].URI != "mr://r1" || in.Resolution != "2K" || in.AspectRatio != "3:4" || in.N != 1 {
		t.Fatalf("input = %+v", in)
	}
	if StripImageOrderLine(in.Prompt) != body || !strings.Contains(in.Prompt, "图2–图5") {
		t.Fatalf("prompt = %q", in.Prompt)
	}
	if req.Labels["kind"] != KindTryon || req.Labels["batch"] != "12" {
		t.Fatalf("labels = %v", req.Labels)
	}
	b, _ := json.Marshal(req)
	if !strings.Contains(string(b), `"operation":"image.edit"`) || !strings.Contains(string(b), `"images":[{"uri":"mr://r1"}`) {
		t.Fatalf("wire = %s", b)
	}
}

func TestBuildImageEditRequestTruncates(t *testing.T) {
	req, warns, err := BuildImageEditRequest(KindImageGen, "img.seedream-5-pro", "画一只猫", refs(12), "1k", "1:1", nil)
	if err != nil || len(warns) != 1 {
		t.Fatal(err, warns)
	}
	in := req.Input.(ImageEditInput)
	if len(in.Images) != 10 || in.Images[9].URI != "mr://r10" || !strings.HasPrefix(in.Prompt, "图序：图1–图10 是参考图。\n") {
		t.Fatalf("seedream cap: %d %q", len(in.Images), in.Prompt)
	}
	// GPT 标称 16，但任何情况都不送 16 张：全局硬上限 14；其余组兜底 14；catalog 给了上限时以它为准（也压到 14）
	req, warns, _ = BuildImageEditRequest(KindImageGen, "img.gpt-image-2", "x", refs(17), "", "", nil)
	if n := len(req.Input.(ImageEditInput).Images); n != 14 || len(warns) != 1 {
		t.Fatalf("gpt cap: %d", n)
	}
	req, warns, _ = BuildImageEditRequest(KindImageGen, "img.nano-banana-2.1", "x", refs(16), "", "", nil)
	if n := len(req.Input.(ImageEditInput).Images); n != 14 || len(warns) != 1 {
		t.Fatalf("default cap: %d", n)
	}
	req, _, _ = BuildImageEditRequestRefs(KindImageGen, "img.nano-banana-2.1", "x", Refs(urisOf(refs(16))), "", "", nil, GenOptions{MaxRefImages: 5})
	if n := len(req.Input.(ImageEditInput).Images); n != 5 {
		t.Fatalf("catalog cap: %d", n)
	}
	if _, _, err := BuildImageEditRequest(KindTryon, "g", "x", nil, "", "", nil); err == nil {
		t.Fatal("no images must fail")
	}
	if _, _, err := BuildImageEditRequest(KindTryon, "g", "x", refs(1), "", "", map[string]string{"Bad Key": "v"}); err == nil {
		t.Fatal("bad label key must fail")
	}
}

func TestBuildImageGenRequest(t *testing.T) {
	req, _, err := BuildImageGenRequest("img.gpt-image-2", "画一只猫", nil, "4K", "16:9", nil)
	if err != nil || req.Operation != OpImageGenerate {
		t.Fatal(err, req.Operation)
	}
	in := req.Input.(ImageGenerateInput)
	if in.Prompt != "画一只猫" || in.Resolution != "4K" || in.AspectRatio != "16:9" {
		t.Fatalf("input = %+v", in)
	}
	req, _, _ = BuildImageGenRequest("img.gpt-image-2", "画一只猫", refs(2), "", "", nil)
	if req.Operation != OpImageEdit {
		t.Fatal("refs → image.edit")
	}
}

func TestParseOutputs(t *testing.T) {
	raw := `{"id":"c1","status":"SUCCEEDED","result":{"operation":"image.edit","outputs":[
		{"index":0,"status":"SUCCEEDED","media":{"rid":"rid-1","url":"https://s/1.png","width":1024,"height":1536,"sha256":"x","bytes":1,"mime":"image/png"}},
		{"index":1,"status":"FAILED","failure":{"class":"CONTENT_POLICY","code":"x"}}]},
		"cost":{"amount":"0.04","currency":"USD","amount_cny":"0.29","source":"price_table","final":true}}`
	var call aigateway.Call
	if err := json.Unmarshal([]byte(raw), &call); err != nil {
		t.Fatal(err)
	}
	out, cost := ParseOutputs(&call)
	if len(out) != 1 || out[0] != (OutputImage{RID: "rid-1", URL: "https://s/1.png", W: 1024, H: 1536}) || cost != 0.29 {
		t.Fatalf("out = %+v cost = %v", out, cost)
	}
	if CostCNY(&aigateway.Cost{Amount: "1.5", Currency: "CNY"}) != 1.5 || CostCNY(&aigateway.Cost{Amount: "1", Currency: "USD"}) != 0 || CostCNY(nil) != 0 {
		t.Fatal("cost fallback")
	}
	if out, cost := ParseOutputs(nil); out != nil || cost != 0 {
		t.Fatal("nil call")
	}
}

func urisOf(list []string) []string { return list }

func TestNormalizeForGroup(t *testing.T) {
	cases := []struct {
		group, res, ar, want string
		warn                 bool
	}{
		{"img.seedream-5-pro", "4k", "1:1", "2k", true},
		{"img.seedream-5-pro", "1k", "3:4", "2k", true},
		{"img.seedream-5-pro", "", "16:9", "2k", true},
		{"img.seedream-5-pro", "1k", "1:1", "1k", false},
		{"img.seedream-5-pro", "2k", "3:4", "2k", false},
		{"img.gpt-image-2", "4k", "3:4", "4k", false},
		{"img.nano-banana-2.1", "", "3:4", "1k", false},
	}
	for _, c := range cases {
		got, warns, err := NormalizeForGroup(c.group, c.res, c.ar)
		if err != nil || got != c.want || (len(warns) > 0) != c.warn {
			t.Errorf("%+v: got %s %v %v", c, got, warns, err)
		}
	}
	req, warns, _ := BuildImageEditRequest(KindPose, "img.seedream-5-pro", "x", []string{"mr://a", "mr://b"}, "4k", "3:4", nil)
	if req.Input.(ImageEditInput).Resolution != "2K" || len(warns) != 1 {
		t.Fatalf("seedream 4k 应降 2K：%+v %v", req.Input, warns)
	}
}

func TestQualityAndAspect(t *testing.T) {
	if QualityFor("img.gpt-image-2", KindTryon, "") != "high" || QualityFor("img.gpt-image-2", KindImageGen, "") != "medium" ||
		QualityFor("img.gpt-image-2", KindImageGen, "LOW") != "low" || QualityFor("img.seedream-5-pro", KindTryon, "high") != "" {
		t.Fatal("quality 规则不对")
	}
	req, _, _ := BuildImageEditRequestRefs(KindTryon, "img.nano-banana-2.1", "x", Refs([]string{"mr://a"}), "", "", nil, GenOptions{Quality: "high"})
	if req.Input.(ImageEditInput).Quality != "" {
		t.Fatal("非 GPT 组不下发 quality")
	}
	req, _, _ = BuildImageEditRequestRefs(KindTryon, "img.gpt-image-2", "x",
		[]aigateway.MediaRef{{URI: "mr://a", Sha256: "abc"}}, "", "3:4", nil, GenOptions{Quality: "high"})
	b, _ := json.Marshal(req)
	if !strings.Contains(string(b), `"quality":"high"`) || !strings.Contains(string(b), `{"uri":"mr://a","sha256":"abc"}`) ||
		!strings.Contains(string(b), `"aspect_ratio":"3:4"`) {
		t.Fatalf("wire = %s", b)
	}
	for _, c := range []struct {
		w, h int
		want string
	}{{1000, 1000, "1:1"}, {768, 1024, "3:4"}, {1080, 1920, "9:16"}, {1200, 800, "3:2"}, {0, 5, ""}} {
		if got := NearestAspectRatio(c.w, c.h); got != c.want {
			t.Errorf("NearestAspectRatio(%d,%d)=%s want %s", c.w, c.h, got, c.want)
		}
	}
	if _, err := checkLabels(map[string]string{"k": strings.Repeat("x", 257)}); err == nil {
		t.Fatal("label 值超 256 应拒")
	}
}

// 2026-11-09 具名档换代：Seedream 5.5 各档比例齐全、支持 4K，不套 5 Pro 的降档规则；只缺 4:5 / 5:4，出图时改最近比例并告警。
func TestSeedream55Group(t *testing.T) {
	const sd55 = "img.seedream-5.5"
	if !IsSeedreamGroup(sd55) || !IsSeedream55Group(sd55) || IsSeedream55Group("img.seedream-5-pro") {
		t.Fatal("Seedream 5.5 组识别不对")
	}
	for _, c := range []struct{ res, ar, want string }{{"4k", "1:1", "4k"}, {"1k", "16:9", "1k"}, {"", "3:4", "1k"}, {"2k", "21:9", "2k"}} {
		got, warns, err := NormalizeForGroup(sd55, c.res, c.ar)
		if err != nil || got != c.want || len(warns) != 0 {
			t.Errorf("%+v: got %s %v %v", c, got, warns, err)
		}
	}
	req, warns, err := BuildImageEditRequest(KindTryon, sd55, "x", []string{"mr://a"}, "4k", "4:5", nil)
	in := req.Input.(ImageEditInput)
	if err != nil || in.Resolution != "4K" || in.AspectRatio != "3:4" || len(warns) != 1 {
		t.Fatalf("5.5 4:5 应改 3:4 并告警、4K 保留：%+v %v %v", in, warns, err)
	}
	gen, warns, err := BuildImageGenerateRequestOpt(sd55, "x", "1k", "5:4", nil, GenOptions{})
	if err != nil || gen.Input.(ImageGenerateInput).AspectRatio != "4:3" || len(warns) != 1 {
		t.Fatalf("5.5 5:4 应改 4:3：%+v %v %v", gen.Input, warns, err)
	}
	// 其它组比例原样（Nano Banana 有 4:5）
	if ar, w := AspectForGroup("img.nano-banana-2.1-dev", "4:5"); ar != "4:5" || w != nil {
		t.Fatalf("非 5.5 组不改比例：%s %v", ar, w)
	}
	if MaxRefImages(sd55) != 10 || MaxRefImages("img.gpt-image-2.6-sunburst") != 16 || MaxRefImages("img.nano-banana-2.1-dev") != 14 {
		t.Fatal("新组的参考图兜底上限不对")
	}
	if QualityFor("img.gpt-image-2.6-sunburst", KindTryon, "") != "high" || QualityFor("img.nano-banana-2.1-dev", KindTryon, "high") != "" {
		t.Fatal("新组 quality 规则不对")
	}
}

func TestMapModelNamedTiersAfterSwap(t *testing.T) {
	groups := map[string]string{"通用": "img.gpt-image-2", "GPT": "img.gpt-image-2.6-sunburst",
		"Banana": "img.nano-banana-2.1-dev", "Seedream": "img.seedream-5.5"}
	for in, want := range map[string]string{
		"GPT": "img.gpt-image-2.6-sunburst", "gpt-image-2.6-sunburst": "img.gpt-image-2.6-sunburst", "gpt-image-2": "img.gpt-image-2.6-sunburst",
		"nano-banana-2.1-dev": "img.nano-banana-2.1-dev", "nano": "img.nano-banana-2.1-dev",
		"seedream-5.5": "img.seedream-5.5", "seedream-5-pro": "img.seedream-5.5", "": "img.gpt-image-2",
	} {
		if got, err := MapModel(in, groups); err != nil || got != want {
			t.Errorf("MapModel(%q)=%q,%v want %q", in, got, err, want)
		}
	}
}
