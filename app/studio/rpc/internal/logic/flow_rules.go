package logic

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/pkg/aigateway"
)

// 本文件是 Poller 的纯判定规则（不碰库、不碰网络），单测见 flow_rules_test.go。

// batchFinalStatus 子任务全部到终态时的批次终态；还有排队 / 生成中的返回 false。
//
//	全部成功 → 全部成功；有成功有失败/取消 → 部分成功；
//	没有成功、有取消（用户取消过） → 已取消（与 cancel 的口径一致）；没有成功、全是失败 → 全部失败。
//
// 取消的子任务在 task_failed 计数里算失败一侧（cancel 同口径）。
func batchFinalStatus(counts map[int8]int) (int8, bool) {
	if counts[model.TaskQueued] > 0 || counts[model.TaskRunning] > 0 || counts[model.TaskDelivering] > 0 {
		return 0, false
	}
	s, f, c := counts[model.TaskSucceeded], counts[model.TaskFailed], counts[model.TaskCanceled]
	switch {
	case s+f+c == 0:
		return 0, false
	case s > 0 && f+c == 0:
		return model.BatchDone, true
	case s > 0:
		return model.BatchPartial, true
	case c > 0:
		return model.BatchCanceled, true
	}
	return model.BatchFailed, true
}

// retrySuffixRe 后台重做后的计费请求号后缀：studio-<task_id>-r<n>。
var retrySuffixRe = regexp.MustCompile(`-r(\d+)$`)

// retryRound 后台重做轮次（从计费请求号后缀取；首轮为 0）。
func retryRound(billingRequestID string) int {
	m := retrySuffixRe.FindStringSubmatch(billingRequestID)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// retryBillingRequestID 后台重做第 n 轮的计费请求号（重新冻结必须换号：旧单已退回不能再确认）。
func retryBillingRequestID(taskID string, n int) string {
	return fmt.Sprintf("%s%s-r%d", BillingPrefix, taskID, n)
}

// idemKey gateway 幂等键 studio:<batch>:<model|pose>:<shot>；后台重做第 n 轮追加 :r<n>
// （同键会拿回旧的终态记录，重做必须换键）。Poller 内的自动重试沿用同键 + retry_failed。
func idemKey(batchID int64, t *model.Task) string {
	idx := t.ModelIndex
	if t.Kind == model.KindPose {
		idx = t.PoseIndex
	}
	k := fmt.Sprintf("studio:%d:%d:%d", batchID, idx, t.ShotIndex)
	if n := retryRound(t.BillingRequestID.String); n > 0 {
		k += fmt.Sprintf(":r%d", n)
	}
	return k
}

// 终态处理动作。
type terminalAction int

const (
	actDeliver terminalAction = iota + 1 // 成功：转存 → 结算 → 交付
	actRetry                             // 可重试的失败：退避后 retry_failed 重开
	actFail                              // 失败 / 取消：退款
	actUnknown                           // 结果未知：标失败但不退款（可能已扣费），待对账
)

// decideTerminal 终态调用 → 动作与（重试时的）等待时长。
// 自动重试次数按 gateway 的 run_no 计（retry_failed 重开 +1），与进程重启无关：run_no=1..3 可再试，共 3 次。
func decideTerminal(call *aigateway.Call) (terminalAction, time.Duration) {
	switch call.Status {
	case aigateway.StatusSucceeded:
		return actDeliver, 0
	case aigateway.StatusUnknown:
		return actUnknown, 0
	case aigateway.StatusFailed:
		if pipeline.RetryableCall(call) {
			if d, ok := pipeline.GenerateRetryDelay(call.RunNo - 1); ok {
				return actRetry, d
			}
		}
	}
	return actFail, 0
}

// 用户可见的失败文案。
const (
	reasonUnknown        = "结果未知，待对账"
	reasonTimeout        = "生成超时，点数已退回"
	reasonSubmitFailed   = "提交生图失败，点数已退回"
	reasonEmptyResult    = "生成结果为空，点数已退回"
	reasonTransfer       = "成品转存失败，点数已退回"
	reasonLost           = "生成记录丢失，点数已退回"
	reasonForceFail      = "运营强制失败，点数已退回"
	reasonGatewayCancel  = "生成已取消，点数已退回"
	reasonSubmitAborted  = "提交未完成，点数已退回"
	reasonDelivering     = "任务正在交付，不能强制失败"
	reasonDeliverTimeout = "交付超时，点数已退回"
	// reasonDeliverOverdue 后台详情里交付中已超过 deliverGrace 的提示（不落库，只透出给运营）
	reasonDeliverOverdue = "交付中已超期，等待结算出口处理"
)

// failureReason gateway 失败 → 用户文案（failure.message 只给运维看，不进文案）。
func failureReason(call *aigateway.Call) string {
	if call == nil {
		return "生成失败，点数已退回"
	}
	if call.Status == aigateway.StatusCancelled {
		return reasonGatewayCancel
	}
	if call.Failure != nil {
		switch call.Failure.Class {
		case "CONTENT_POLICY":
			return "内容未通过审核，点数已退回"
		case "INPUT_REJECTED":
			return "输入图片不符合要求，点数已退回"
		case "DEADLINE_EXCEEDED":
			return reasonTimeout
		case "NO_ROUTE", "NO_CAPACITY", "ATTEMPTS_EXHAUSTED":
			return "生图服务繁忙，点数已退回"
		}
	}
	return "生成失败，点数已退回"
}

// costCNYText 上游成本 → cost_cny 列（DECIMAL 文本）。以 gateway 的 amount_cny 为准；
// amount_cny 为空且币种是 CNY 时退用 amount；其余无法确定人民币金额的，返回 "0" 且 ok=false（调用方记 warning）。
func costCNYText(c *aigateway.Cost) (text string, ok bool) {
	if c == nil {
		return "0", false
	}
	s := strings.TrimSpace(c.AmountCNY)
	if s == "" && strings.EqualFold(strings.TrimSpace(c.Currency), "CNY") {
		s = strings.TrimSpace(c.Amount)
	}
	if s == "" {
		return "0", false
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return "0", false
	}
	return s, true
}

// orchestrateMaxFailures 批次编排遇到可重试错误（sidecar / 媒体服务暂不可用）时，最多跨轮重来的次数；
// 超过就整批失败退款。次数从 step_log 里 step=retry 的条数取，进程重启不丢。
const orchestrateMaxFailures = 3

// stepLogRetries step_log 里编排重试的条数：只数最近一次后台重做（admin_retry）之后的，重做后重新计数。
func stepLogRetries(logs []model.StepLog) int {
	n := 0
	for _, l := range logs {
		switch l.Step {
		case stepRetry:
			n++
		case stepAdminRetry:
			n = 0
		}
	}
	return n
}

const (
	stepRetry      = "retry"
	stepAdminRetry = "admin_retry"
	stepRewrite    = "rewrite"
	stepFinished   = "finished"
	stepFailed     = "failed"
)

// batchStatusName 批次状态名（step_log detail 用）。
func batchStatusName(s int8) string {
	switch s {
	case model.BatchQueued:
		return "queued"
	case model.BatchCropping:
		return "cropping"
	case model.BatchPrompting:
		return "prompting"
	case model.BatchGenerating:
		return "generating"
	case model.BatchDone:
		return "done"
	case model.BatchFailed:
		return "failed"
	case model.BatchPartial:
		return "partial"
	case model.BatchCanceled:
		return "canceled"
	case model.BatchSubmitting:
		return "submitting"
	}
	return strconv.Itoa(int(s))
}
