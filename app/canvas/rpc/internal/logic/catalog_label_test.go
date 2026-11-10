package logic

import (
	"slices"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/config"
)

// 目录图片档顺序：Seedream 5.5 排在 Seedream 5.0 Pro 前；label 一律是完整模型名，不是 Image / Banana / SD5P 这类 wire 值。
func TestCatalogImageModelsOrderAndLabels(t *testing.T) {
	c := testCore()
	var ids, labels []string
	for _, m := range c.s.Config.ModelTable() {
		if m.Kind != "image" {
			continue
		}
		cm := c.catalogModel(m)
		ids = append(ids, cm.ModelId)
		labels = append(labels, cm.Label)
		if cm.Model != m.Model {
			t.Errorf("%s: wire 值 model 不应改动：%q", m.ModelID, cm.Model)
		}
	}
	wantIDs := []string{"image-v1", "gpt-image-2-6-v1", "banana-v1", "seedream-5-5-v1", "sd5p-v1"}
	wantLabels := []string{"GPT Image 2.5", "GPT Image 2.6", "Nano Banana 2.1", "Seedream 5.5", "Seedream 5.0 Pro"}
	if !slices.Equal(ids, wantIDs) || !slices.Equal(labels, wantLabels) {
		t.Fatalf("图片档 ids=%v labels=%v，want %v / %v", ids, labels, wantIDs, wantLabels)
	}
	for _, m := range c.s.Config.ModelTable() {
		if m.Kind != "video" {
			continue
		}
		if l := c.catalogModel(m).Label; l == "" || !strings.HasPrefix(l, "Seedance 2.") {
			t.Errorf("%s: 视频档 label=%q", m.ModelID, l)
		}
	}
}

// Seedream 5.5 档：网关组 img.seedream-5.5（命中 canvas_image_seedream_5_5 计费档位）、≤10 张参考图、1K/2K/4K、没有 4:5 / 5:4。
// Seedream 5.0 Pro 档：img.seedream-5-pro、只给 2K。
func TestSeedreamTiersSpec(t *testing.T) {
	c := testCore()
	sd55, ok := c.s.Config.FindModel("", "seedream-5-5-v1", "image")
	if !ok || sd55.Model != "SD55" || sd55.ModelGroup != "img.seedream-5.5" || sd55.MaxImageRefs != 10 ||
		!slices.Equal(sd55.Sizes, []string{"1K", "2K", "4K"}) || slices.Contains(sd55.Ratios, "4:5") || slices.Contains(sd55.Ratios, "5:4") {
		t.Fatalf("SD55: %+v", sd55)
	}
	sp, err := c.buildSpec(seedream55Req("2K"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ch := c.charge(7, "r", sp); ch.ModelGroup != "img.seedream-5.5" || ch.Resolution != "2K" {
		t.Fatalf("计费应按 img.seedream-5.5 选档：%+v", ch)
	}
	sd5p, ok := c.s.Config.FindModel("SD5P", "", "image")
	if !ok || sd5p.ModelGroup != "img.seedream-5-pro" || !slices.Equal(sd5p.Sizes, []string{"2K"}) {
		t.Fatalf("SD5P: %+v", sd5p)
	}
}

// 用户可见名：错误文案、素材名都用完整模型名；模型表里没有的值按 pkg/modellabel 映射。
func TestModelDisplayNames(t *testing.T) {
	c := testCore()
	for in, want := range map[string]string{"Image": "GPT Image 2.5", "GPT": "GPT Image 2.5", "Banana": "Nano Banana 2.1",
		"SD5P": "Seedream 5.0 Pro", "SD55": "Seedream 5.5", "GPT Image 2.6": "GPT Image 2.6", "Seedance Fast": "Seedance 2.0 Fast"} {
		m, ok := c.s.Config.FindModel(in, "", "")
		if !ok || modelDisplay(m) != want {
			t.Errorf("modelDisplay(%q)=%q want %q", in, modelDisplay(m), want)
		}
	}
	if got := c.runModelLabel(model.KindImage, "", storedRequest{Model: "Banana", ModelID: "banana-v1"}); got != "Nano Banana 2.1" {
		t.Errorf("素材名模型=%q", got)
	}
	// 换组前受理的 SD5P run（冻结 Seedream 5.5）：素材名按实际出图的组
	if got := c.runModelLabel(model.KindImage, "img.seedream-5.5", storedRequest{Model: "SD5P", ModelID: "sd5p-v1"}); got != "Seedream 5.5" {
		t.Errorf("换组前的 SD5P 素材名=%q", got)
	}
	if got := c.runModelLabel(model.KindToolTryon, "", storedRequest{Model: "通用"}); got != "通用" {
		t.Errorf("试穿通用档=%q", got)
	}
	if got := c.runModelLabel(model.KindToolCrop, "", storedRequest{}); got != "" {
		t.Errorf("无模型应为空：%q", got)
	}
	// 自定义配置没写 Label：按模型组登记名推导
	if got := (config.ModelSpec{Model: "X", ModelID: "x-v1", ModelGroup: "img.seedream-5.5"}).DisplayName(); got != "Seedream 5.5" {
		t.Errorf("DisplayName 推导=%q", got)
	}
}
