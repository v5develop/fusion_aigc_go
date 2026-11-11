package logic

// gateway 后端 run 的超时判定（软时限 Timeouts.* / 硬上限 Timeouts.*HardCap，都从 run 创建算起）。
//
// 背景：软时限包含提交阶段（参考图上传给供应商就可能用掉 1–3 分钟）。到点就取消会出现「供应商已受理、已扣费、马上出图，
// 我们却取消并给用户退款」——钱付给了供应商，用户什么也没拿到；而受理之后供应商不会因为取消停止计费，取消省不下钱。
//
//	软时限前                        → 照常等
//	过了硬上限                       → 失败退款 + 取消全部调用（与改造前超时一致）；count>1 已有成品时改为取消其余调用、
//	                                   按成功张数收尾（宽限 hardCapGrace 内仍不到终态才整单失败退款）
//	过了软时限、有调用已被供应商受理 → 不取消已受理的，继续等到硬上限；同 run 里还没受理的调用照旧取消（count>1 部分成功按成功张数结算）
//	过了软时限、没有任何调用受理     → 失败退款 + 取消（与改造前一致）
//	过了软时限、有调用状态拿不到     → 本轮不判（宁可多等一轮，也不在不知道是否已受理时取消），硬上限兜底
//
// 退款仍只经 failRun 的 CAS 一次。

import (
	"context"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"

	"github.com/zeromicro/go-zero/core/logx"
)

// acceptedStates 供应商已受理、还在进行中（可能已扣费、可能马上出图）的尝试状态。
var acceptedStates = map[string]bool{"ACCEPTED": true, "POLLING": true, "RECONCILING": true, "REHOSTING": true}

// attemptEnded 尝试已到终态（SUCCEEDED / FAILED / UNKNOWN）：它带的 accepted_at 只说明「曾经受理过」，不说明还会出图。
func attemptEnded(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "UNKNOWN":
		return true
	}
	return false
}

// callAccepted 未终态调用（本轮运行）是否有**仍在进行**的尝试已被供应商受理：调用阶段已在轮询 / 对账 / 转存，
// 或本轮有未终态的尝试带 accepted_at / 处于受理后的状态。
//
// 不看已结束的尝试：上一个部署受理后失败（ACCEPTED_THEN_FAILED）、网关正换部署重试（ROUTING / WAITING_CAPACITY）时，
// 那次受理不会再出图，按它延长只会让用户白等到硬上限。money_at_risk 也覆盖这类已失败的尝试（以及以前的运行），
// 只在网关没给尝试明细时作兜底信号。
func callAccepted(c *aigateway.Call) bool {
	if c == nil {
		return false
	}
	switch c.Phase {
	case "POLLING", "RECONCILING", "REHOSTING":
		return true
	}
	if len(c.Attempts) == 0 {
		return c.MoneyAtRisk
	}
	for _, a := range c.Attempts {
		if c.RunNo > 0 && a.RunNo > 0 && a.RunNo != c.RunNo {
			continue
		}
		if attemptEnded(a.State) {
			continue
		}
		if a.AcceptedAt != nil || acceptedStates[a.State] {
			return true
		}
	}
	return false
}

// timeoutVerdict 超时判定结果。
type timeoutVerdict struct {
	fail   bool     // 置失败退款并取消全部调用
	cancel []string // 只取消这些（未受理的），run 继续等已受理的
	extend bool     // 已过软时限，因有已受理调用（或状态未知）继续等
}

// hardCapGrace 过了硬上限、已有成品时，取消其余调用后再等多久让它们到终态（按成功张数收尾）；再不到终态就整单失败退款。
const hardCapGrace = 2 * time.Minute

// gatewayTimeout ids 与 calls 一一对应（calls[i] 为 nil = 本轮没拿到该调用的状态）。
func (c *core) gatewayTimeout(run *model.NodeRun, ids []string, calls []*aigateway.Call) timeoutVerdict {
	if !c.expired(run) {
		return timeoutVerdict{}
	}
	accepted, unknown, delivered := false, false, false
	var unaccepted, open []string
	for i, id := range ids {
		var call *aigateway.Call
		if i < len(calls) {
			call = calls[i]
		}
		switch {
		case call == nil:
			unknown = true
			open = append(open, id)
		case call.IsTerminal():
			delivered = delivered || call.Succeeded()
		case callAccepted(call):
			accepted = true
			open = append(open, id)
		default:
			unaccepted = append(unaccepted, id)
			open = append(open, id)
		}
	}
	if age := c.now().Sub(run.CreatedAt); age > c.hardCap(run.Kind) {
		// 硬上限：count>1 已有成品时不把已出的图连同退款一起丢掉——其余调用全部取消，到终态后按成功张数收尾
		// （ClassifyGateway：成功 + 已取消 → 成功，actual=成功张数）；宽限期内仍不到终态才整单失败退款。
		if delivered && age <= c.hardCap(run.Kind)+hardCapGrace {
			return timeoutVerdict{extend: true, cancel: open}
		}
		return timeoutVerdict{fail: true}
	}
	switch {
	case accepted:
		return timeoutVerdict{extend: true, cancel: unaccepted}
	case delivered && !unknown:
		// 已有成品：未受理的取消掉，取消到终态后按成功张数收尾（不把已出的图连同退款一起丢掉）
		return timeoutVerdict{extend: true, cancel: unaccepted}
	case unknown:
		return timeoutVerdict{extend: true}
	}
	return timeoutVerdict{fail: true}
}

// hardDeadline run 的硬上限时刻（日志用）。
func (c *core) hardDeadline(run *model.NodeRun) time.Time {
	return run.CreatedAt.Add(c.hardCap(run.Kind))
}

// applyGatewayTimeout 按判定处理。返回 true 表示 run 已结束（调用方不再继续）。
func (c *core) applyGatewayTimeout(ctx context.Context, run *model.NodeRun, ids []string, v timeoutVerdict) bool {
	switch {
	case v.fail:
		if c.failRun(ctx, run, model.RunRunning, "TIMEOUT", "生成超时，点数已退回", true) {
			c.cancelCalls(ctx, run.ID, ids, "生成超时")
		}
		return true
	case v.extend:
		if len(v.cancel) > 0 {
			c.cancelCalls(ctx, run.ID, v.cancel, "生成超时（未受理）")
		}
		logx.WithContext(ctx).Infof("canvas: run %d 已过软时限，有调用已被供应商受理（或状态未知），不取消，最多等到 %s (check=canvas.timeout_extended)",
			run.ID, c.hardDeadline(run).Format(time.RFC3339))
	}
	return false
}
