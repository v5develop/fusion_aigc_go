package logic

import (
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/config"
)

// 画布冻结带上本次的质量与参考图张数（按请求参数冻结，Java V1.0.223）：质量与派发同一取值，参考图 = 派发送的张数。
func TestChargeCarriesImageUsage(t *testing.T) {
	c := &core{}
	gpt26 := config.ModelSpec{Qualities: []string{"high", "medium", "low"}}
	cases := []struct {
		name    string
		sp      runSpec
		quality string
		refs    int
	}{
		{"GPT 2.6 没选质量 → high（与派发同，2026-11-19 起缺省 high）", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.6-sunburst", Size: "1K"}, "high", 0},
		{"GPT 2.6 high + 3 张参考图", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.6-sunburst", Size: "1K", Quality: "high",
			Images: []refMedia{{URL: "a"}, {URL: "b"}, {URL: "c"}}}, "high", 3},
		{"GPT 2.5 标准版支持画质（2026-11-19）", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.5-sunburst", Size: "1K", Quality: "low"}, "low", 0},
		{"Banana 不发 quality", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.nano-banana-2.1", Size: "1K", Quality: "high"}, "", 0},
	}
	for _, tc := range cases {
		sp := tc.sp
		ch := c.charge(7, "r", &sp)
		if ch.Usage == nil || ch.Usage.Quality != tc.quality || ch.Usage.RefImages != tc.refs {
			t.Errorf("%s: %+v", tc.name, ch.Usage)
		}
		if got := imageQuality(&sp); got != tc.quality {
			t.Errorf("%s: 派发质量 %q 与计费 %q 不一致", tc.name, got, tc.quality)
		}
	}
	// 视频不带用量
	if ch := c.charge(7, "r", &runSpec{Kind: model.KindVideo, DurationS: 5, Size: "720p"}); ch.Usage != nil {
		t.Fatalf("视频不应带用量: %+v", ch.Usage)
	}
}

// 编辑（带参考图）按请求画质计价与下发，不再固定 medium；low 也透传。
func TestChargeEditQualityPassthrough(t *testing.T) {
	c := &core{}
	gpt26 := config.ModelSpec{Qualities: []string{"high", "medium", "low"}}
	for _, q := range []string{"low", "medium", "high"} {
		sp := runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.6-sunburst", Size: "1K", Quality: q, Images: []refMedia{{URL: "a"}}}
		ch := c.charge(7, "r", &sp)
		if ch.Usage.Quality != q || imageQuality(&sp) != q {
			t.Errorf("%s: 计价 %q 派发 %q", q, ch.Usage.Quality, imageQuality(&sp))
		}
	}
}

func TestDefaultQualityHigh(t *testing.T) {
	if got := (config.ModelSpec{Qualities: []string{"low", "medium", "high"}}).DefaultQuality(); got != "high" {
		t.Errorf("含 high 缺省 high: %q", got)
	}
	if got := (config.ModelSpec{}).DefaultQuality(); got != "" {
		t.Errorf("无画质模型应为空: %q", got)
	}
	if got := (config.ModelSpec{Qualities: []string{"medium", "low"}}).DefaultQuality(); got != "medium" {
		t.Errorf("无 high 取首项: %q", got)
	}
	for _, m := range config.DefaultModels {
		if len(m.Qualities) > 0 && m.Qualities[0] != "high" {
			t.Errorf("%s 目录画质应 high 在前: %v", m.Model, m.Qualities)
		}
	}
}
