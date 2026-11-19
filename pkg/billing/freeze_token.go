package billing

import (
	"encoding/json"
	"math/big"
	"strings"
)

// 按 token 估算冻结（GPT Image 2.5 标准版 / 2.6，契约 §AE 修订）：冻结贴近 Atlas 真实费用，不按最贵组合。
//
// freeze_cap_json 里出现 "token_model" 段即启用本公式（没有则走 freeze_cap.go 的旧「画质 × 面积」公式）。
// 字段名【与 Java ActualCostFreezeCap 对齐前为暂定】，对齐点见各字段注释；公式全程有理数、中间不取整（token 数除外），
// Java 用 BigDecimal 同序计算即逐位一致。
//
//	文字 token   = ceil(提示词字符数 / text_chars_per_token) + text_overhead_tokens
//	参考图 token = Σ ceil(ref_base_tokens × min(像素, ref_max_pixels) / ref_base_pixels)
//	               像素 = 参考图 宽×高（网关上传前等比缩小到 ≤ ref_max_pixels，只缩不放，token 按缩小后线性放大）；
//	               像素拿不到（0 / 未传）→ 按 ref_max_pixels（缩小后的上限，最坏值）
//	输出 token   = output_tokens[画质][档位]（可配置的「实测典型输出 token 表」；画质缺失按 default_quality，档位缺失 → 不封顶）
//	每张美元     = (文字 × price_per_mtok.text_in + 参考图 × price_per_mtok.image_in + 输出 × price_per_mtok.image_out) / 1e6
//	               × rate_factor（校准系数，贴近实际费用，初值 0.35） × (1 + buffer)（安全垫，默认 0.15）
//	每张点数     = max(1, ceil(美元 × 汇率 × 加成 × 100))（FairPoints，与结算同一汇率 / 加成）
//
// 一次出图一次调用（每张各带全部参考图和提示词），所以上面是「每张」；张数由计费内核乘。
// 冻结仍取 min(价目表单价, 上限)；结算按实际成本且不超过冻结额（超出部分不再扣，见 SettleCost 的指标 billing_actual_cost_over_freeze）。

// TokenModel 解析后的 token 估算参数。
type TokenModel struct {
	PriceTextIn, PriceImageIn, PriceImageOut   *big.Rat // 美元 / 百万 token
	TextCharsPerToken                          *big.Rat // 默认 2（中文 1 字约 1 token，英文 4 字符约 1 token，取保守）
	TextOverheadTokens                         int64
	RefBaseTokens, RefBasePixels, RefMaxPixels int64 // 2.5 / 2.6：1756 token @ 1024²，上限 2048²
	OutputTokens                               map[string]map[string]int64
	RateFactor, Buffer                         *big.Rat
}

func parseTokenModel(raw json.RawMessage) (*TokenModel, bool) {
	var j struct {
		Price struct {
			TextIn   json.RawMessage `json:"text_in"`
			ImageIn  json.RawMessage `json:"image_in"`
			ImageOut json.RawMessage `json:"image_out"`
		} `json:"price_per_mtok"`
		CharsPerToken json.RawMessage             `json:"text_chars_per_token"`
		Overhead      int64                       `json:"text_overhead_tokens"`
		RefBaseTokens int64                       `json:"ref_base_tokens"`
		RefBasePixels int64                       `json:"ref_base_pixels"`
		RefMaxPixels  int64                       `json:"ref_max_pixels"`
		Output        map[string]map[string]int64 `json:"output_tokens"`
		RateFactor    json.RawMessage             `json:"rate_factor"`
		Buffer        json.RawMessage             `json:"buffer"`
	}
	if json.Unmarshal(raw, &j) != nil {
		return nil, false
	}
	tm := &TokenModel{TextOverheadTokens: j.Overhead, RefBaseTokens: j.RefBaseTokens, RefBasePixels: j.RefBasePixels,
		RefMaxPixels: j.RefMaxPixels, OutputTokens: map[string]map[string]int64{}}
	var ok bool
	pos := func(r json.RawMessage) *big.Rat {
		v, good := decimalOf(r)
		if !good || v.Sign() <= 0 {
			ok = false
		}
		return v
	}
	ok = true
	tm.PriceTextIn, tm.PriceImageIn, tm.PriceImageOut = pos(j.Price.TextIn), pos(j.Price.ImageIn), pos(j.Price.ImageOut)
	if !ok || tm.RefBaseTokens < 0 || tm.RefBasePixels <= 0 || tm.RefMaxPixels <= 0 || tm.TextOverheadTokens < 0 || len(j.Output) == 0 {
		return nil, false
	}
	tm.TextCharsPerToken = big.NewRat(2, 1)
	if s := strings.TrimSpace(string(j.CharsPerToken)); s != "" && s != "null" {
		v, good := decimalOf(j.CharsPerToken)
		if !good || v.Sign() <= 0 {
			return nil, false
		}
		tm.TextCharsPerToken = v
	}
	tm.RateFactor = big.NewRat(35, 100)
	if s := strings.TrimSpace(string(j.RateFactor)); s != "" && s != "null" {
		v, good := decimalOf(j.RateFactor)
		if !good || v.Sign() <= 0 {
			return nil, false
		}
		tm.RateFactor = v
	}
	tm.Buffer = big.NewRat(15, 100)
	if s := strings.TrimSpace(string(j.Buffer)); s != "" && s != "null" {
		v, good := decimalOf(j.Buffer)
		if !good || v.Sign() < 0 {
			return nil, false
		}
		tm.Buffer = v
	}
	for q, tiers := range j.Output {
		m := map[string]int64{}
		for t, n := range tiers {
			if n <= 0 {
				return nil, false
			}
			m[lowerTrim(t)] = n
		}
		tm.OutputTokens[lowerTrim(q)] = m
	}
	return tm, true
}

// refTokens 一张参考图的 token（像素 <= 0 视为未知，按缩小后的上限）。
func (tm *TokenModel) refTokens(pixels int64) int64 {
	if pixels <= 0 || pixels > tm.RefMaxPixels {
		pixels = tm.RefMaxPixels
	}
	return ceilDiv(tm.RefBaseTokens*pixels, tm.RefBasePixels)
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// perImageUSD 见文件注释；档位 / 画质取不到 → nil, false。
func (tm *TokenModel) perImageUSD(fc *FreezeCap, resolution string, u ImageUsage) (*big.Rat, bool) {
	q := lowerTrim(u.Quality)
	if q == "" || tm.OutputTokens[q] == nil {
		q = fc.DefaultQuality
	}
	out, ok := tm.OutputTokens[q][lowerTrim(resolution)]
	if !ok {
		return nil, false
	}
	chars := big.NewRat(int64(max(u.PromptChars, 0)), 1)
	textTok := ceilRat(new(big.Rat).Quo(chars, tm.TextCharsPerToken)) + tm.TextOverheadTokens
	var refTok int64
	switch {
	case len(u.RefPixels) > 0:
		for _, px := range u.RefPixels {
			refTok += tm.refTokens(px)
		}
	default:
		for i := 0; i < max(u.RefImages, 0); i++ {
			refTok += tm.refTokens(0)
		}
	}
	sum := new(big.Rat).Mul(big.NewRat(textTok, 1), tm.PriceTextIn)
	sum.Add(sum, new(big.Rat).Mul(big.NewRat(refTok, 1), tm.PriceImageIn))
	sum.Add(sum, new(big.Rat).Mul(big.NewRat(out, 1), tm.PriceImageOut))
	sum.Quo(sum, big.NewRat(1_000_000, 1))
	sum.Mul(sum, tm.RateFactor)
	sum.Mul(sum, new(big.Rat).Add(big.NewRat(1, 1), tm.Buffer))
	return sum, true
}
