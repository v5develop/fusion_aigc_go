package logic

import (
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/config"
)

// 画布冻结带上本次的质量与参考图张数（按请求参数冻结，Java V1.0.223）：质量与派发同一取值，参考图 = 派发送的张数。
func TestChargeCarriesImageUsage(t *testing.T) {
	c := &core{}
	gpt26 := config.ModelSpec{Qualities: []string{"low", "medium", "high"}}
	cases := []struct {
		name    string
		sp      runSpec
		quality string
		refs    int
	}{
		{"GPT 2.6 没选质量 → medium（与派发同）", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.6-sunburst", Size: "1K"}, "medium", 0},
		{"GPT 2.6 high + 3 张参考图", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.6-sunburst", Size: "1K", Quality: "high",
			Images: []refMedia{{URL: "a"}, {URL: "b"}, {URL: "c"}}}, "high", 3},
		{"GPT 2.5 不发 quality", runSpec{Kind: model.KindImage, Model: gpt26, ModelGroup: "img.gpt-image-2.5-sunburst", Size: "1K", Quality: "high"}, "", 0},
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
