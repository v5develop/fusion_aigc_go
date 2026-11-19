package logic

import (
	"strconv"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/config"
)

// 安全阀 max_cost 不低于网关预检的保守上限估算：2.5 标准版 4K high + 3 张参考图（约 $1.84 ≈ 16 元）不能被固定 5 元卡住。
func TestMaxCostCoversGatewayPrecheck(t *testing.T) {
	m25, _ := (config.Config{}).FindModel("GPT Image 2.5", "", "image")
	m26, _ := (config.Config{}).FindModel("GPT Image 2.6", "", "image")
	imgs := func(n int) []refMedia { return make([]refMedia, n) }
	cny := func(sp *runSpec) float64 {
		v, err := strconv.ParseFloat(imageMaxCostCNY(sp, "5.00", "7.30"), 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	sp := &runSpec{Kind: model.KindImage, Model: m25, ModelGroup: m25.ModelGroup, Size: "4K", Quality: "high", Images: imgs(3)}
	if !strings.HasSuffix(m25.ModelGroup, "-std") {
		t.Fatalf("2.5 应是标准版组: %s", m25.ModelGroup)
	}
	// 网关预检：(1000×5 + 3×7024×8 + 55561×30) / 1e6 ≈ $1.8425 → 最低 ≈ 13.45 元（不含余量）
	need := (1000*5.0 + 3*7024*8.0 + 55561*30.0) / 1e6 * 7.30
	if got := cny(sp); got < need*1.2-0.02 || got <= 5 {
		t.Errorf("2.5 4K high 3 参考图 max_cost=%v 低于预检 %v（含 1.2 余量）", got, need*1.2)
	}
	// 2.6 4K high：输出 20006 token × $32/M
	sp26 := &runSpec{Kind: model.KindImage, Model: m26, ModelGroup: m26.ModelGroup, Size: "4K", Quality: "high", Images: imgs(14)}
	need26 := (1000*5.0 + 14*7024*8.0 + 20006*32.0) / 1e6 * 7.30
	if got := cny(sp26); got < need26*1.2-0.02 {
		t.Errorf("2.6 4K high 14 参考图 max_cost=%v < %v", got, need26*1.2)
	}
	// 小请求不低于默认 5 元；按张计价的模型（Banana）原样用默认值
	small := &runSpec{Kind: model.KindImage, Model: m25, ModelGroup: m25.ModelGroup, Size: "1K", Quality: "low"}
	if got := cny(small); got < 5 {
		t.Errorf("不能低于默认值: %v", got)
	}
	ban, _ := (config.Config{}).FindModel("Banana", "", "image")
	if got := imageMaxCostCNY(&runSpec{Kind: model.KindImage, Model: ban, ModelGroup: ban.ModelGroup, Size: "2K"}, "5.00", "7.30"); got != "5.00" {
		t.Errorf("Banana 用默认值: %s", got)
	}
	// 随画质 / 尺寸 / 参考图单调
	hi := cny(&runSpec{Kind: model.KindImage, Model: m25, ModelGroup: m25.ModelGroup, Size: "4K", Quality: "high"})
	if hi <= cny(&runSpec{Kind: model.KindImage, Model: m25, ModelGroup: m25.ModelGroup, Size: "4K", Quality: "medium"}) {
		t.Error("high 应高于 medium")
	}
}

// xhigh / max（2026-11-19）：与网关同口径暂定 2× / 4× high 的输出 token，max_cost 必须覆盖网关预检。
func TestMaxCostCoversXhighAndMax(t *testing.T) {
	m25, _ := (config.Config{}).FindModel("GPT Image 2.5", "", "image")
	m26, _ := (config.Config{}).FindModel("GPT Image 2.6", "", "image")
	cny := func(m config.ModelSpec, size, q string, refs int) float64 {
		v, err := strconv.ParseFloat(imageMaxCostCNY(&runSpec{Kind: model.KindImage, Model: m, ModelGroup: m.ModelGroup, Size: size,
			Quality: q, Images: make([]refMedia, refs)}, "5.00", "7.30"), 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name string
		m    config.ModelSpec
		size string
		q    string
		refs int
		out  float64 // 网关预检的输出 token
		rate float64
	}{
		{"2.5 4K max 3 参考图", m25, "4K", "max", 3, 222244, 30},  // ceil(28096 × 8294400 / 1048576)
		{"2.5 2K xhigh", m25, "2K", "xhigh", 0, 57619, 30},     // ceil(14048 × 4300800 / 1048576)
		{"2.6 4K max 14 参考图", m26, "4K", "max", 14, 80022, 32}, // ceil(28452 × √7.91)
		{"2.6 1K xhigh", m26, "1K", "xhigh", 0, 14226, 32},
	}
	for _, c := range cases {
		need := (1000*5.0 + float64(c.refs)*7024*8.0 + c.out*c.rate) / 1e6 * 7.30 * 1.2
		if got := cny(c.m, c.size, c.q, c.refs); got < need-0.02 {
			t.Errorf("%s: max_cost=%v 低于预检 %v（含 1.2 余量）", c.name, got, need)
		}
	}
	for _, m := range []config.ModelSpec{m25, m26} {
		h, x, mx := cny(m, "4K", "high", 0), cny(m, "4K", "xhigh", 0), cny(m, "4K", "max", 0)
		if !(h < x && x < mx) {
			t.Errorf("%s 应 high < xhigh < max: %v %v %v", m.Model, h, x, mx)
		}
	}
}
