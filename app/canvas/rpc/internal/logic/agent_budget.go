package logic

// canvas_run_node 的点数上限（max_points，2026-11-14）：冻结前用与冻结同源的报价比较，超出则不冻结、不提交。
//
//   - canvas（gateway：image / video / i2p）：quoteGateway → billing.Service.Quote(c.charge(...))，与 holdAndQueue 冻结同一个 Charge；
//     按量结算（ACTUAL_COST）取冻结上限；计费关闭时报价为 0。
//   - studio（tryon）：quoteStudio（studio-rpc Quote，与提交同一套归一），取 max_points（没有则 points）。
//   - java（upscale / outpaint / mask）：拿得到该用户 Bearer（user / svc）且 Java 有只读报价接口时，用 Java 报价（与提交冻结同源）比较；
//     拿不到（mcp_token、旧版 Java、报价失败）→ 沿用 expected_points 规则，缺失 → PRICE_UNKNOWN_NEEDS_CONFIRM。
//   - free（crop）：0。
//
// 报价本身不可用（准入不通过等）时不在这里拦，交给 RunNode 后续的准入报真实原因；报价暂时失败（price_known=false）→ PRICE_UNKNOWN_NEEDS_CONFIRM（可重试）。
// 只对智能体调用生效（agentRunNode 把上限放进 ctx）；网页手动执行不受影响。幂等回放 / 去重回放不冻结，也不比较。

import (
	"context"
	"fmt"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/model"

	"github.com/zeromicro/go-zero/core/logx"
)

type runBudget struct {
	Max         int64
	Expected    int64
	HasExpected bool
}

type budgetKey struct{}

func withBudget(ctx context.Context, b runBudget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

func budgetFrom(ctx context.Context) (runBudget, bool) {
	b, ok := ctx.Value(budgetKey{}).(runBudget)
	return b, ok
}

// agentBudgetArgs 从工具参数取 max_points / expected_points；没给 max_points → ok=false（不设上限）。
func agentBudgetArgs(args map[string]any) (runBudget, bool) {
	mx, ok := args["max_points"]
	if !ok || mx == nil {
		return runBudget{}, false
	}
	b := runBudget{Max: int64(toInt(mx))}
	if ev, ok := args["expected_points"]; ok && ev != nil {
		b.Expected, b.HasExpected = int64(toInt(ev)), true
	}
	return b, true
}

func priceUnknown(msg string, retryable bool) *agentError {
	return agentErr(agenttool.ErrPriceUnknownNeedsConfirm, msg, retryable)
}

// checkBudget RunNode 冻结前调用：ctx 里没有上限直接放行。
func (c *core) checkBudget(ctx context.Context, userID int64, sp *runSpec) error {
	b, ok := budgetFrom(ctx)
	if !ok {
		return nil
	}
	q := &quoteOut{Warnings: []string{}}
	q.setBackend(sp.Backend)
	var quoted int64
	switch q.Billing {
	case "java":
		if pts, ok := c.javaBudgetQuote(ctx, userID, sp); ok {
			quoted = pts
			break
		}
		if !b.HasExpected {
			return priceUnknown("这个工具由主站按其价目扣点，画布报不出准确点数；带 max_points 时须同时给 expected_points（估价），或先征得用户确认后不带 max_points 执行", false)
		}
		quoted = b.Expected
	case "canvas", "studio":
		if q.Billing == "canvas" {
			c.quoteGateway(ctx, userID, sp, q)
		} else {
			c.quoteStudio(ctx, userID, sp, q)
		}
		if q.Available != nil && !*q.Available {
			return nil // 准入 / 报价不可用：交给后续准入报真实原因
		}
		if !q.PriceKnown {
			return priceUnknown("暂时无法报价，无法确认是否在 max_points 之内；请稍后重试，或先征得用户确认后不带 max_points 执行", true)
		}
		// 与 RunNode 出参 credits / max_points 同口径（影子模式也按价目比较，计费关闭时报价本就是 0）
		quoted = max(q.MaxPoints, q.Points)
	}
	if quoted > b.Max {
		logx.WithContext(ctx).Infof("canvas agent: 拒绝执行 user=%d billing=%s（check=agent_run.budget quoted=%d max_points=%d）", userID, q.Billing, quoted, b.Max)
		e := agentErr(agenttool.ErrBudgetExceeded, fmt.Sprintf("这次执行预计 %d 点，超过了上限 %d 点，没有执行", quoted, b.Max), false)
		e.QuotedPoints, e.MaxPoints = quoted, b.Max
		return e
	}
	return nil
}

// javaBudgetQuote Java 工具的准确报价（与 canvas_quote_node 同一请求体、同一接口）；报不出返回 ok=false。
func (c *core) javaBudgetQuote(ctx context.Context, userID int64, sp *runSpec) (int64, bool) {
	if viaMcpToken(ctx) {
		return 0, false
	}
	path, body, err := c.javaToolBody(ctx, &model.NodeRun{Kind: sp.Kind}, sp, true)
	if err != nil {
		return 0, false
	}
	jq, err := c.javaQuote(ctx, userID, path, body)
	if err != nil {
		return 0, false
	}
	return jq.Points, true
}
