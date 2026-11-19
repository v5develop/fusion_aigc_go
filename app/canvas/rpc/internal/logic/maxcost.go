package logic

// limits.max_cost（人民币）：网关的防异常安全阀，不是用户扣费，更不能按冻结点数换算。
//
// 网关预检用「保守上限估算」判断一次调用会不会超 max_cost，超了直接拒绝。按 token 计价的生图模型（GPT Image 2.5 标准版 / 2.6）
// 的上限随画质 × 尺寸 × 参考图数暴涨（2.5 的 4K high 约 $1.67 ≈ 12 元），固定 5 元会把正常请求拒掉。
// 取法：复刻网关的 token 上限表（与 ai-gateway adapters/atlas.py 同口径，每个模型一套）估算本次每张调用的美元上限，
// × 美元汇率（Config.MaxCostCNY.UsdCNY，偏保守）× 1.2 余量，向上取到分；再与配置的默认值（Image，5 元）取大 ——
// 只会抬高、不会低于原来的行为。其余模型 / 工具仍用配置值。

import (
	"fmt"
	"math"
	"math/big"
	"strings"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/modelgroup"
)

const (
	maxCostMargin    = 1.2  // 估算余量
	gwTextTokens     = 1000 // 网关每张输出按 1000 文字 token 预扣
	gwRefBaseTokens  = 1756 // 每张参考图 1024² 的 token
	gwRefMaxPixels   = 2048 * 2048
	gwBasePixels     = 1024 * 1024
	gwTextPerMtok    = 5.0
	gwImageInPerMtok = 8.0
)

// 网关输出 token 上限表（1024² 基准）。
var (
	// xhigh / max 与网关同口径暂定 2× / 4× high（Atlas 只说「high 之上、更贵」，上线后按实测校准）
	gw25Out      = map[string]int{"low": 196, "medium": 1756, "high": 7024, "xhigh": 7024 * 2, "max": 7024 * 4}  // 2.5：其它尺寸按面积线性放大
	gw26Out      = map[string]int{"low": 1991, "medium": 3160, "high": 7113, "xhigh": 7113 * 2, "max": 7113 * 4} // 2.6：其它尺寸按面积比开方放大且不低于基准
	gwTierPixels = map[string]int{"1K": 1048576, "2K": 4300800, "4K": 8294400}
)

// gatewayCostUSDUpper 本次每张调用在网关预检里的美元上限；不是这两个按 token 计价的模型返回 0, false。
func gatewayCostUSDUpper(group, size, quality string, refs int) (float64, bool) {
	var (
		table  map[string]int
		outMtk float64
		sqrtIt bool
	)
	switch {
	case modelgroup.Equal(group, modelgroup.GPTImage25SunburstStd):
		table, outMtk = gw25Out, 30
	case strings.EqualFold(group, "img.gpt-image-2.6-sunburst"):
		table, outMtk, sqrtIt = gw26Out, 32, true
	default:
		return 0, false
	}
	q := strings.ToLower(quality)
	base, ok := table[q]
	if !ok {
		base = table["high"] // 画质缺失按缺省档 high（与派发一致：imageQuality 缺省 high，网关也不会收到更高档）
	}
	px, ok := gwTierPixels[strings.ToUpper(size)]
	if !ok {
		px = gwTierPixels["4K"]
	}
	ratio := float64(px) / gwBasePixels
	out := float64(base) * ratio
	if sqrtIt {
		out = math.Ceil(float64(base) * math.Max(1, math.Sqrt(ratio)))
	} else {
		out = math.Ceil(out)
	}
	// 参考图：像素未知，按网关缩小后的上限（最坏 2048²）
	refTok := float64(refs) * math.Ceil(float64(gwRefBaseTokens)*float64(gwRefMaxPixels)/gwBasePixels)
	usd := (float64(gwTextTokens)*gwTextPerMtok + refTok*gwImageInPerMtok + out*outMtk) / 1e6
	return usd, true
}

// imageMaxCostCNY 生图 limits.max_cost；token 计价模型按上限估算（≥ 默认值），其余原样返回 def。
func imageMaxCostCNY(sp *runSpec, def, usdCNY string) string {
	usd, ok := gatewayCostUSDUpper(sp.ModelGroup, sp.Size, imageQuality(sp), len(sp.Images))
	if !ok {
		return def
	}
	rate := 7.30
	if r, ok := new(big.Rat).SetString(strings.TrimSpace(usdCNY)); ok && r.Sign() > 0 {
		rate, _ = r.Float64()
	}
	cny := math.Ceil(usd*rate*maxCostMargin*100) / 100
	if d, ok := new(big.Rat).SetString(strings.TrimSpace(def)); ok {
		if df, _ := d.Float64(); df > cny {
			return def
		}
	}
	return fmt.Sprintf("%.2f", cny)
}

func (c *core) maxCostFor(k model.RunKind, sp *runSpec) string {
	def := c.maxCost(k)
	if k == model.KindImage && sp != nil {
		return imageMaxCostCNY(sp, def, c.s.Config.MaxCostCNY.UsdCNY)
	}
	return def
}
