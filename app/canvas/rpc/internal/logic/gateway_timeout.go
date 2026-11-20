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
// 判「失败」时**先**取消调用、再置失败：网关对已发给供应商的调用拒绝取消（409 CANCEL_NOT_ALLOWED，受理即扣费、取消省不下钱），
// 只要有调用被拒，就不失败退款，run 保持运行中等这些调用到终态，再按部分成功规则收尾（ClassifyGateway：成功几张交付几张、
// 按成功张数 / 实报成本结算退差额；一张没成功按失败退款；含 UNKNOWN 待对账）；没发出去的调用照常取消（零成本）。
// 等待上限：run 创建 + 硬上限 + Timeouts.VendorSentWait（vendorSentWaitUntil），过了仍未终态才按超时失败退款并打 ERROR
// （check=canvas.vendor_sent_wait_exceeded）。不需要额外状态：每轮判失败都会重新取消一次，仍被拒就继续等。
//
// 退款仍只经 failRun 的 CAS 一次。

import (
	"context"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskevent"

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
	// open 判定时仍未终态（含状态未知）的调用：判失败时只取消这些，已 CANCELLED / 终态的不再重复发 Cancel
	open   []string
	extend bool // 已过软时限，因有已受理调用（或状态未知）继续等
	// reason 判定依据（写进任务轨迹的 reason 属性）：soft_cap_none_accepted / soft_cap_accepted_extended /
	// soft_cap_partial_delivered / soft_cap_status_unknown / hard_cap / hard_cap_partial_grace
	reason string
	// attrs 判定时的现场（已耗时、软 / 硬上限、已受理 / 未受理 / 未知的调用数）
	attrs taskevent.Attrs
}

// 超时区间的 attempt：延长判定与最终失败判定是同一 step_path 下的两次，用不同序号避免 dedupe 互相吞掉。
const (
	timeoutExtendSeq = 1
	timeoutFailSeq   = 2
)

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
	_, soft := c.budget(run.Kind)
	info := taskevent.Attrs{
		"elapsed_ms": c.now().Sub(run.CreatedAt).Milliseconds(), "soft_cap_s": int64(soft / time.Second),
		"hard_cap_s": int64(c.hardCap(run.Kind) / time.Second), "calls": len(ids), "unaccepted": len(unaccepted),
		"accepted": accepted, "unknown": unknown, "delivered": delivered,
	}
	if age := c.now().Sub(run.CreatedAt); age > c.hardCap(run.Kind) {
		// 硬上限：count>1 已有成品时不把已出的图连同退款一起丢掉——其余调用全部取消，到终态后按成功张数收尾
		// （ClassifyGateway：成功 + 已取消 → 成功，actual=成功张数）；宽限期内仍不到终态才整单失败退款。
		if delivered && age <= c.hardCap(run.Kind)+hardCapGrace {
			return timeoutVerdict{extend: true, cancel: open, reason: "hard_cap_partial_grace", attrs: info}
		}
		return timeoutVerdict{fail: true, open: open, reason: "hard_cap", attrs: info}
	}
	switch {
	case accepted:
		return timeoutVerdict{extend: true, cancel: unaccepted, reason: "soft_cap_accepted_extended", attrs: info}
	case delivered && !unknown:
		// 已有成品：未受理的取消掉，取消到终态后按成功张数收尾（不把已出的图连同退款一起丢掉）
		return timeoutVerdict{extend: true, cancel: unaccepted, reason: "soft_cap_partial_delivered", attrs: info}
	case unknown:
		return timeoutVerdict{extend: true, reason: "soft_cap_status_unknown", attrs: info}
	}
	return timeoutVerdict{fail: true, open: open, reason: "soft_cap_none_accepted", attrs: info}
}

// hardDeadline run 的硬上限时刻（日志用）。
func (c *core) hardDeadline(run *model.NodeRun) time.Time {
	return run.CreatedAt.Add(c.hardCap(run.Kind))
}

// applyGatewayTimeout 按判定处理。返回 true 表示 run 已结束（调用方不再继续）。
func (c *core) applyGatewayTimeout(ctx context.Context, run *model.NodeRun, ids []string, v timeoutVerdict) bool {
	switch {
	case v.fail:
		attrs := runAttrs(run, v.attrs)
		attrs["reason"] = v.reason
		attrs["decision"] = "fail"
		sfx := func(status string) taskevent.SpanOpt { return taskevent.WithDedupeSuffix(status) }
		// 先取消再判：被拒（已发给供应商）的调用不失败退款，等它到终态按部分成功收尾（见文件头）
		toCancel := v.open
		if toCancel == nil { // 判定没给（老调用方 / 测试直接构造）：按全部调用
			toCancel = ids
		}
		if refused := c.cancelCalls(ctx, run.ID, toCancel, "生成超时"); len(refused) > 0 {
			until := c.vendorSentWaitUntil(run)
			if !c.now().After(until) {
				c.applyGatewayTimeout(ctx, run, ids, timeoutVerdict{extend: true, reason: "vendor_sent_wait", attrs: v.attrs})
				return false
			}
			logx.WithContext(ctx).Errorf("canvas: run %d 超时取消被网关拒绝的调用 %s 等到 %s 仍未终态，按超时失败退款（供应商可能已扣费，产物若晚到由网关记账） (check=canvas.vendor_sent_wait_exceeded)",
				run.ID, strings.Join(refused, ","), until.Format(time.RFC3339))
			attrs["reason"] = "vendor_sent_wait_exceeded"
		}
		c.evStart(ctx, run, stTimeout, timeoutFailSeq, attrs)
		if c.failRun(ctx, run, model.RunRunning, "TIMEOUT", "生成超时，点数已退回", true) {
			c.evEnd(ctx, run, stTimeout, timeoutFailSeq, taskevent.StatusTimeout,
				taskevent.Fail{Class: "TIMEOUT", Code: "TIMEOUT", Check: "canvas.gateway_timeout", Message: "生成超时（" + v.reason + "），已失败退款并取消调用"}, attrs, sfx(taskevent.StatusTimeout))
		} else {
			// 状态进 dedupe：这次没抢到（skipped）不会吞掉之后真正失败的 timeout
			c.evEnd(ctx, run, stTimeout, timeoutFailSeq, taskevent.StatusSkipped,
				taskevent.Fail{Class: "LOCAL", Code: "STATE_CHANGED", Check: "canvas.gateway_timeout", Message: "超时判定时 run 已被别处推进"}, attrs, sfx(taskevent.StatusSkipped))
		}
		return true
	case v.extend:
		attrs := runAttrs(run, v.attrs)
		attrs["reason"] = v.reason
		attrs["decision"] = "extend"
		attrs["cancel_unaccepted"] = len(v.cancel)
		attrs["hard_deadline"] = c.hardDeadline(run).Format(time.RFC3339)
		if v.reason == "vendor_sent_wait" {
			attrs["wait_until"] = c.vendorSentWaitUntil(run).Format(time.RFC3339)
		}
		if len(v.cancel) > 0 {
			c.cancelCalls(ctx, run.ID, v.cancel, "生成超时（未受理）")
		}
		// 每个轮询 tick 都会再判一次：只在原因变化时才写轨迹和日志（原因进 dedupe，A→B→A 也各记一次），在取消调用之后写
		n, changed := timeoutReasons.changed(run.ID, v.reason)
		if changed {
			sfx := taskevent.WithDedupeSuffix(v.reason + "#" + strconv.Itoa(n))
			c.evStart(ctx, run, stTimeout, timeoutExtendSeq, attrs, sfx)
			c.evEnd(ctx, run, stTimeout, timeoutExtendSeq, taskevent.StatusSkipped,
				taskevent.Fail{Class: "TIMEOUT", Code: "TIMEOUT_EXTENDED", Check: "canvas.timeout_extended", Message: "已过软时限，不取消，继续等待（" + v.reason + "）"}, attrs, sfx)
		}
		if !changed {
			return false
		}
		until := c.hardDeadline(run)
		if v.reason == "vendor_sent_wait" {
			until = c.vendorSentWaitUntil(run)
		}
		logx.WithContext(ctx).Infof("canvas: run %d 已超时，有调用已被供应商受理（或状态未知 / 网关拒绝取消），不失败，最多等到 %s (check=canvas.timeout_extended reason=%s)",
			run.ID, until.Format(time.RFC3339), v.reason)
	}
	return false
}
