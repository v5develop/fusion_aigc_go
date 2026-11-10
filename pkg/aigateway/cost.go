package aigateway

import (
	"math/big"
	"strings"
)

// 成本来源（Cost.Source）。只有 CostSourceReported 且 Final=true 的才是供应商实报的终值，
// 可以拿来按实际用量结算用户点数；configured / estimated / billed 都只是网关按价目估的或尚未定稿的数。
const (
	CostSourceReported   = "reported"
	CostSourceConfigured = "configured"
	CostSourceEstimated  = "estimated"
	CostSourceBilled     = "billed"
)

// 尝试的扣费状态（Attempt.Charged）。
const (
	ChargedYes     = "CHARGED"
	ChargedNo      = "NOT_CHARGED"
	ChargedUnknown = "UNKNOWN"
)

// Authoritative 是否是可用于结算的实报终值：source=reported 且 final=true。
func (c *Cost) Authoritative() bool {
	return c != nil && c.Source == CostSourceReported && c.Final
}

// usd 实报终值的美元金额。不是实报终值、币种不是 USD、金额解析不了都返回 false。
func (c *Cost) usd() (*big.Rat, bool) {
	if !c.Authoritative() || !strings.EqualFold(strings.TrimSpace(c.Currency), "USD") {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(c.Amount))
	if !ok || r.Sign() < 0 {
		return nil, false
	}
	return r, true
}

// ReportedUSD 一组调用（同一笔预留下的全部 gateway 调用）的供应商实报成本合计（美元）。
//
// 每个调用按这个顺序取：
//  1. 调用级 cost 是实报终值（source=reported && final）→ 用它（网关已把各次尝试合计好）；
//  2. 否则逐个尝试求和：NOT_CHARGED 的不计；其余（CHARGED / UNKNOWN）每个都必须带实报终值，缺一个整组就不可用；
//     一个都没扣费的失败调用记 0。
//
// 任一调用为 nil（没查到）或取不到实报终值 → ok=false，调用方按冻结价全额确认（判据 ACTUAL_COST_UNAVAILABLE），
// 不拿估算值去结算。
func ReportedUSD(calls []*Call) (*big.Rat, bool) {
	if len(calls) == 0 {
		return nil, false
	}
	sum := new(big.Rat)
	for _, c := range calls {
		v, ok := callUSD(c)
		if !ok {
			return nil, false
		}
		sum.Add(sum, v)
	}
	return sum, true
}

func callUSD(c *Call) (*big.Rat, bool) {
	if c == nil {
		return nil, false
	}
	if v, ok := c.Cost.usd(); ok {
		return v, true
	}
	if c.Succeeded() {
		// 成功的调用必定扣过费：调用级没有实报终值时，只有每个扣费尝试都实报了才能合计
		if len(c.Attempts) == 0 {
			return nil, false
		}
	} else if !c.IsTerminal() {
		return nil, false // 还在跑，成本没定
	}
	sum := new(big.Rat)
	charged := false
	for _, a := range c.Attempts {
		if a.Charged == ChargedNo {
			continue
		}
		v, ok := a.Cost.usd()
		if !ok {
			return nil, false
		}
		charged = true
		sum.Add(sum, v)
	}
	if !charged && c.Succeeded() {
		return nil, false // 成功却没有任何扣费尝试：数据不全，不敢当 0
	}
	return sum, true
}
