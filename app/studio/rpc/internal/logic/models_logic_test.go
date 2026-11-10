package logic

import (
	"context"
	"slices"
	"strings"
	"testing"

	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"
)

func capOf(t *testing.T, items []*studio.ModelCapability, code string) *studio.ModelCapability {
	t.Helper()
	for _, it := range items {
		if it.Code == code {
			return it
		}
	}
	t.Fatalf("缺模型档 %s", code)
	return nil
}

// 目录拉不到：只按组规则；顺序固定，Seedream 5.5 无 4:5 / 5:4，GPT 2.6 参考图按 kind 收紧。
func TestModelCapabilitiesRulesOnly(t *testing.T) {
	items := modelCapabilities("imagegen", config.DefaultModelGroups, nil)
	var codes []string
	for _, it := range items {
		codes = append(codes, it.Code)
	}
	if !slices.Equal(codes, []string{"通用", "GPT", "GPT25", "Banana", "Seedream"}) {
		t.Fatalf("顺序 %v", codes)
	}
	sd := capOf(t, items, "Seedream")
	if sd.Label != "Seedream 5.5" || slices.Contains(sd.AspectRatios, "4:5") || slices.Contains(sd.AspectRatios, "5:4") ||
		!slices.Equal(sd.Resolutions, []string{"1k", "2k", "4k"}) || len(sd.AspectRatios) != 8 || sd.AutoAspect {
		t.Fatalf("Seedream %+v", sd)
	}
	gpt := capOf(t, items, "GPT")
	if gpt.Label != "GPT Image 2.6" || gpt.MaxRefImages != 5 || len(gpt.AspectRatios) != 10 {
		t.Fatalf("GPT %+v", gpt)
	}
	if g := capOf(t, items, "通用"); g.Label != "通用" || !g.Available || g.MaxRefImages != MaxImageGenRefs {
		t.Fatalf("通用 %+v", g)
	}
	if b := capOf(t, items, "Banana"); b.Label != "Nano Banana 2.1" {
		t.Fatalf("Banana %+v", b)
	}
	// 商拍：GPT 2.6 编辑上限 6，比例有「自动」
	tryon := capOf(t, modelCapabilities("tryon", config.DefaultModelGroups, nil), "GPT")
	if tryon.MaxRefImages != 6 || !tryon.AutoAspect {
		t.Fatalf("tryon GPT %+v", tryon)
	}
}

// 旧 Seedream 5 Pro：无 4K。
func TestModelCapabilitiesLegacySeedream(t *testing.T) {
	sd := capOf(t, modelCapabilities("imagegen", map[string]string{"Seedream": "img.seedream-5-pro"}, nil), "Seedream")
	if !slices.Equal(sd.Resolutions, []string{"1k", "2k"}) || len(sd.AspectRatios) != 10 || sd.MaxRefImages != 9 {
		t.Fatalf("%+v", sd)
	}
}

// 目录：求交（大小写不敏感）、目录参考图上限优先、目录里没有的组标不可用、交集为空忽略目录。
func TestModelCapabilitiesCatalogIntersect(t *testing.T) {
	four := 4
	cat := map[string]aigateway.ModelGroup{
		"img.nano-banana-2.1": {Code: "img.nano-banana-2.1", Enabled: true, FeatureLimits: aigateway.FeatureLimits{
			Resolutions: []string{"1K", "2K"}, AspectRatios: []string{"1:1", "16:9", "7:3"}, MaxReferenceImages: &four}},
		"img.seedream-5.5": {Code: "img.seedream-5.5", Enabled: true, FeatureLimits: aigateway.FeatureLimits{Resolutions: []string{"1024x1024"}}},
	}
	groups := map[string]string{"Banana": "img.nano-banana-2.1", "Seedream": "img.seedream-5.5", "GPT": "img.gpt-image-2.6-sunburst"}
	items := modelCapabilities("imagegen", groups, cat)
	b := capOf(t, items, "Banana")
	if !slices.Equal(b.Resolutions, []string{"1k", "2k"}) || !slices.Equal(b.AspectRatios, []string{"1:1", "16:9"}) || b.MaxRefImages != 4 || !b.Available {
		t.Fatalf("Banana %+v", b)
	}
	if sd := capOf(t, items, "Seedream"); len(sd.Resolutions) != 3 {
		t.Fatalf("交集为空应忽略目录 %+v", sd)
	}
	if g := capOf(t, items, "GPT"); g.Available {
		t.Fatalf("目录里没有的组应不可用 %+v", g)
	}
}

func TestModelsRejectsBadKind(t *testing.T) {
	if _, err := NewModelsLogic(context.Background(), &svc.ServiceContext{}).Models(&studio.ModelsReq{Kind: "x"}); err == nil {
		t.Fatal("非法 kind 应拒绝")
	}
}

// 不接计费 / gateway：按组规则返回，with_price 时每档分辨率都有价位行（计费关为 0 点）。
func TestModelsWithPriceBillingOff(t *testing.T) {
	out, err := NewModelsLogic(context.Background(), &svc.ServiceContext{}).Models(&studio.ModelsReq{UserId: 7, WithPrice: true})
	if err != nil || out.Kind != "imagegen" || len(out.Items) != 5 || out.BillingMode != "off" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	for _, it := range out.Items {
		if len(it.Prices) != len(it.Resolutions) || !it.Prices[0].Available || it.Prices[0].UnitPoints != 0 {
			t.Fatalf("%s prices=%+v", it.Code, it.Prices)
		}
	}
}

// 没登记展示名的组：label 回落到档位码，不用 gateway 目录的 display_name（可能带供应商措辞）。
func TestModelCapabilitiesLabelIgnoresCatalogDisplayName(t *testing.T) {
	groups := map[string]string{"NewOne": "img.unlisted-x"}
	cat := map[string]aigateway.ModelGroup{"img.unlisted-x": {Code: "img.unlisted-x", DisplayName: "某供应商 X 模型", Enabled: true}}
	out := modelCapabilities("imagegen", groups, cat)
	if len(out) != 1 || out[0].Label != "NewOne" {
		t.Fatalf("label=%q，应回落档位码", out[0].Label)
	}
}

// 对外档名只有 GPT25（不出现 GPT25Dev）；网关目录只登记旧 Dev 组码（网关先于规范组发布前）也能命中，不误标不可用。
func TestModelCapabilitiesCanonicalCodeAndLegacyCatalog(t *testing.T) {
	cat := map[string]aigateway.ModelGroup{
		"img.gpt-image-2.5-sunburst-developer": {Code: "img.gpt-image-2.5-sunburst-developer", Enabled: true},
		"img.nano-banana-2.1-dev":              {Code: "img.nano-banana-2.1-dev", Enabled: true},
	}
	groups := map[string]string{"通用": "img.gpt-image-2.5-sunburst", "GPT25": "img.gpt-image-2.5-sunburst", "Banana": "img.nano-banana-2.1"}
	items := modelCapabilities("imagegen", groups, cat)
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Code+it.Label+it.ModelGroup), "dev") {
			t.Fatalf("输出不应带 dev：%+v", it)
		}
		if !it.Available {
			t.Fatalf("旧组码目录应命中：%+v", it)
		}
	}
	if g := capOf(t, items, "GPT25"); g.Label != "GPT Image 2.5" {
		t.Fatalf("GPT25 %+v", g)
	}
}
