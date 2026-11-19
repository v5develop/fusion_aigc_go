package logic

import (
	"reflect"
	"testing"
	"time"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"
)

// 预估与冻结同口径：同样的输入，estimateSpec 造出的 Charge 与 RunNode 受理（specModelParams）造出的逐字段相同。
func TestEstimateChargeEqualsFreezeCharge(t *testing.T) {
	c := &core{s: &svc.ServiceContext{Config: testConfig()}, now: time.Now}
	cases := []struct {
		name string
		req  *canvas.EstimateReq
		q    string
		refs int
		size string
	}{
		{"GPT 2.6 缺省画质 high + 3 张参考图", &canvas.EstimateReq{Model: "GPT Image 2.6", Size: "2K", Count: 2, RefImages: 3}, "high", 3, "2K"},
		{"GPT 2.6 edit low", &canvas.EstimateReq{ModelId: "gpt-image-2-6-v1", Size: "1K", Count: 1, Quality: "low", RefImages: 1}, "low", 1, "1K"},
		{"GPT 2.5 标准版 high + 编辑", &canvas.EstimateReq{Model: "Image", Size: "4K", Count: 1, Quality: "high", RefImages: 2}, "high", 2, "4K"},
		{"GPT 2.5 缺省画质 high", &canvas.EstimateReq{Model: "GPT Image 2.5", Size: "1K", Count: 1}, "high", 0, "1K"},
		{"Banana 固定价：画质清空", &canvas.EstimateReq{Model: "Banana", Size: "1K", Count: 1, Quality: "high", RefImages: 2}, "", 2, "1K"},
	}
	for _, tc := range cases {
		tc.req.PromptChars = 1 // 受理侧提示词 "x"
		sp, err := c.estimateSpec(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		est := c.charge(9, "quote", sp)

		// RunNode 受理路径（specModelParams）造出的 runSpec
		m := sp.Model
		real := &runSpec{Prompt: "x", Kind: sp.Kind, Model: m, ModelGroup: m.ModelGroup, EntryCode: sp.EntryCode}
		for i := 0; i < tc.refs; i++ {
			real.Images = append(real.Images, refMedia{URL: "u"})
		}
		if err := c.specModelParams(real, &canvas.RunNodeRequest{Prompt: "x", Size: tc.req.Size, Count: tc.req.Count, Quality: tc.req.Quality}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		frz := c.charge(9, "quote", real)
		if est.Usage == nil || frz.Usage == nil || !reflect.DeepEqual(*est.Usage, *frz.Usage) || est.Usage.Quality != tc.q || est.Usage.RefImages != tc.refs {
			t.Errorf("%s: 预估 %+v 冻结 %+v 期望画质 %q", tc.name, est.Usage, frz.Usage, tc.q)
		}
		if est.Resolution != frz.Resolution || est.ModelGroup != frz.ModelGroup || est.Count != frz.Count || est.Resolution != tc.size {
			t.Errorf("%s: 预估 %+v 冻结 %+v", tc.name, est, frz)
		}
	}
}

func TestEstimateRejectsBadInput(t *testing.T) {
	c := &core{s: &svc.ServiceContext{Config: testConfig()}, now: time.Now}
	for _, r := range []*canvas.EstimateReq{
		{Model: "不存在"},
		{Model: "GPT Image 2.6", Size: "8K"},
		{Model: "GPT Image 2.6", Count: 99},
		{Model: "GPT Image 2.6", RefImages: 100},
	} {
		if _, err := c.estimateSpec(r); err == nil {
			t.Errorf("%+v 应拒绝", r)
		}
	}
}

func TestEstimateLimiter(t *testing.T) {
	l := &estimateLimiter{hits: map[int64][]time.Time{}}
	now := time.Now()
	for i := 0; i < estimateLimitPerMin; i++ {
		if !l.allow(1, now) {
			t.Fatalf("第 %d 次不应被限", i)
		}
	}
	if l.allow(1, now) || !l.allow(2, now) {
		t.Fatal("超限应拒绝，且按用户隔离")
	}
	if !l.allow(1, now.Add(61*time.Second)) {
		t.Fatal("窗口过后应放行")
	}
}

// 参考图像素：请求给的 width/height 进计价（受理与估价同口径），没给的按未知（0，冻结时取缩小后上限）。
func TestRefDimsFlowIntoChargeAndEstimate(t *testing.T) {
	c := &core{s: &svc.ServiceContext{Config: testConfig()}, now: time.Now}
	if w, h := refDims(&canvas.RunReference{Width: 1536, Height: 1024}, nil); w != 1536 || h != 1024 {
		t.Errorf("请求宽高: %d x %d", w, h)
	}
	if w, h := refDims(&canvas.RunReference{Width: 1536}, nil); w != 0 || h != 0 {
		t.Errorf("缺一边应未知: %d x %d", w, h)
	}
	if w, h := refDims(&canvas.RunReference{Width: 70000, Height: 10}, nil); w != 0 || h != 0 {
		t.Errorf("越界应未知: %d x %d", w, h)
	}
	sp, err := c.estimateSpec(&canvas.EstimateReq{Model: "GPT Image 2.5", Size: "2K", PromptChars: 10,
		RefImageSizes: []*canvas.ImageSize{{W: 1536, H: 1024}, {W: 0, H: 0}}, RefImages: 1})
	if err != nil {
		t.Fatal(err)
	}
	u := c.charge(9, "q", sp).Usage
	if len(u.RefPixels) != 2 || u.RefPixels[0] != 1536*1024 || u.RefPixels[1] != 0 || u.RefImages != 2 {
		t.Fatalf("estimate 的像素: %+v", u)
	}
	// 受理路径：refMedia 带宽高 → Usage.RefPixels
	acc := &runSpec{Kind: sp.Kind, Model: sp.Model, ModelGroup: sp.ModelGroup, Size: "1K", Quality: "high",
		Images: []refMedia{{URL: "a", W: 800, H: 600}, {URL: "b"}}}
	if got := c.charge(9, "q", acc).Usage.RefPixels; len(got) != 2 || got[0] != 800*600 || got[1] != 0 {
		t.Fatalf("受理像素: %v", got)
	}
}
