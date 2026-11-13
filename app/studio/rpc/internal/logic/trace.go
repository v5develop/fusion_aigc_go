package logic

// 任务轨迹（afv_task_event）：studio 子任务（商拍 / 姿态 / 图片生成）的生命周期写进与 Java 共用的事件表。
// task_id = taskcenter.Ref(svc.Prefix(cfg, kind), 子任务主键)，即任务中心同步进 afv_creation_task 的编号（不是 afv_studio_task.task_id）。
// 契约见 pkg/taskevent；写入尽力而为，从不影响业务路径。
//
// step_path 一览：
//
//	go.studio/queue               排队（start 的时刻取子任务创建时间）→ 派发提交 / 在排队中终结
//	go.studio/gateway_call/0      向网关创建调用（attempt=第几轮；external_ref=调用号）+ gateway_call_linked
//	go.studio/wait                等待网关出结果（到终态 / 失败 / 超时 / 重试换下一轮）
//	go.studio/timeout             生图超时判定（timeout，带耗时与时限）
//	go.studio/rehost              同步转存素材库
//	go.studio/deliver             结算 + 置成功交付（失败 / 下一轮重试都收尾）
//	go.studio/refund              失败 / 取消的退款
//	go.studio/cancel              取消请求（mark）

import (
	"context"
	"strconv"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/taskevent"
)

const (
	stQueue    = "go.studio/queue"
	stCall     = "go.studio/gateway_call/0"
	stWait     = "go.studio/wait"
	stTimeout  = "go.studio/timeout"
	stRehost   = "go.studio/rehost"
	stDeliver  = "go.studio/deliver"
	stRefund   = "go.studio/refund"
	stCancelMk = "go.studio/cancel"
)

// studioTraceID 子任务 → 创作任务号。
func studioTraceID(cfg config.Config, t *model.Task) string {
	return taskcenter.Ref(svc.Prefix(cfg, t.Kind), t.ID)
}

func taskAttrs(t *model.Task, extra taskevent.Attrs) taskevent.Attrs {
	a := taskevent.Attrs{"kind": string(t.Kind)}
	if t.ModelGroup != "" {
		a["model_group"] = t.ModelGroup
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// nowSeq 没有持久计数器的重试区间的 attempt 序号（毫秒时间，同一次尝试的 start / end 共用）。
func nowSeq() int { return int(time.Now().UnixMilli() % 1_000_000_000) }

// callAttempt 网关调用 / 等待区间的 attempt：第几轮（retry_failed 重开时是下一轮，attempt 列在重开成功后才 +1）。
func callAttempt(t *model.Task, retryFailed bool) int {
	n := int(t.Attempt) + 1
	if retryFailed {
		n++
	}
	return n
}

// ---- flow ----

func (f *flow) evStart(ctx context.Context, t *model.Task, step string, attempt int, attrs taskevent.Attrs, opts ...taskevent.SpanOpt) {
	if f.ev != nil {
		f.ev.Start(ctx, studioTraceID(f.cfg, t), step, attempt, taskAttrs(t, attrs), opts...)
	}
}

func (f *flow) evEnd(ctx context.Context, t *model.Task, step string, attempt int, status string, fl taskevent.Fail, attrs taskevent.Attrs, opts ...taskevent.SpanOpt) {
	if f.ev != nil {
		f.ev.Finish(ctx, studioTraceID(f.cfg, t), step, attempt, status, fl, taskAttrs(t, attrs), opts...)
	}
}

// evQueueDone 排队区间收尾：start 取子任务创建时间，end 取当前（派发或在排队里终结）。
func (f *flow) evQueueDone(ctx context.Context, t *model.Task, status string, fl taskevent.Fail) {
	if f.ev == nil {
		return
	}
	tid := studioTraceID(f.cfg, t)
	f.ev.Write(ctx, taskevent.Event{TaskID: tid, TS: t.CreatedAt, StepPath: stQueue, Kind: taskevent.KindSpanStart, Attrs: taskAttrs(t, nil)})
	// 结局进 dedupe：失败后重新排队、之后顺利派发（或反过来）两次收尾都记，不会被先到的吞掉；start 仍只留一条
	f.ev.Finish(ctx, tid, stQueue, 0, status, fl, taskAttrs(t, taskevent.Attrs{"queued_ms": f.now().Sub(t.CreatedAt).Milliseconds()}),
		taskevent.WithDedupeSuffix(status))
}

// evWaitFail 等待区间以失败 / 超时 / 取消收尾；reason 是子任务的失败原因文案（已是面向用户的话，不含地址与凭证）。
func (f *flow) evWaitFail(ctx context.Context, t *model.Task, reason, gwStatus string) {
	status, code := taskevent.StatusFailed, "TASK_FAILED"
	if reason == reasonTimeout {
		status, code = taskevent.StatusTimeout, "TIMEOUT"
	}
	a := taskevent.Attrs{"gateway_status": gwStatus}
	if t.StartedAt.Valid {
		a["elapsed_ms"] = f.now().Sub(t.StartedAt.Time).Milliseconds()
	}
	if t.GatewayCallID.Valid {
		a["call_known"] = true
	}
	f.evEnd(ctx, t, stWait, callAttempt(t, false), status,
		taskevent.Fail{Class: "LOCAL", Code: code, Check: "studio.task_failed", Message: reason}, a, taskevent.WithDedupeSuffix(status))
}

// evTimeout 生图超时：go.studio/timeout 以 timeout 收尾（reason 说明取消调用的结果）。
func (f *flow) evTimeout(ctx context.Context, t *model.Task, gwStatus, reason string) {
	a := taskevent.Attrs{
		"reason": reason, "gateway_status": gwStatus, "limit_s": int64(f.generateTimeout() / time.Second), "decision": "fail",
	}
	if t.StartedAt.Valid {
		a["elapsed_ms"] = f.now().Sub(t.StartedAt.Time).Milliseconds()
	}
	f.evStart(ctx, t, stTimeout, 1, a)
	f.evEnd(ctx, t, stTimeout, 1, taskevent.StatusTimeout,
		taskevent.Fail{Class: "TIMEOUT", Code: "TIMEOUT", Check: "studio.generate_timeout", Message: "生图超时（" + reason + "），已失败退款"}, a)
}

// evRefund 退款区间（refundTask 内部调用）：区间在 bill.Fail 调用之后才收尾，status=ok 只表示「已请求退款」（result=requested）。
func (f *flow) evRefund(ctx context.Context, t *model.Task, reason string, run func()) {
	if f.ev == nil || t == nil {
		run()
		return
	}
	a := taskevent.Attrs{"reason": reason}
	f.evStart(ctx, t, stRefund, 0, a)
	run()
	f.evEnd(ctx, t, stRefund, 0, taskevent.StatusOK, taskevent.Fail{}, taskevent.Attrs{"reason": reason, "result": "requested"})
}

// ---- deps（用户取消）----

// evCancel 取消排队任务：取消 mark + 排队区间以 cancelled 收尾。refundable 只表示「将要请求退款」；
// 退款区间由调用方在 bill.Fail 之后用 evRefundRequested 写，这里不声称退款已经发生。
func (d *deps) evCancel(ctx context.Context, t *model.Task, refundable bool) {
	if d.ev == nil {
		return
	}
	tid := studioTraceID(d.cfg, t)
	a := taskAttrs(t, taskevent.Attrs{"refund_requested": refundable})
	d.ev.Mark(ctx, tid, stCancelMk, "用户取消排队中的任务", 0, a)
	d.ev.Write(ctx, taskevent.Event{TaskID: tid, TS: t.CreatedAt, StepPath: stQueue, Kind: taskevent.KindSpanStart, Attrs: taskAttrs(t, nil)})
	d.ev.Finish(ctx, tid, stQueue, 0, taskevent.StatusCancelled,
		taskevent.Fail{Class: "LOCAL", Code: "CANCELED", Check: "studio.cancel", Message: "用户取消"}, a, taskevent.WithDedupeSuffix(taskevent.StatusCancelled))
}

// evRefundRequested 取消路径的退款区间：必须在 bill.Fail 调用之后写；ok = 已请求退款（result=requested）。
func (d *deps) evRefundRequested(ctx context.Context, t *model.Task, reason string) {
	if d.ev == nil {
		return
	}
	tid := studioTraceID(d.cfg, t)
	a := taskAttrs(t, taskevent.Attrs{"reason": reason, "result": "requested"})
	d.ev.Start(ctx, tid, stRefund, 0, a)
	d.ev.Finish(ctx, tid, stRefund, 0, taskevent.StatusOK, taskevent.Fail{}, a)
}

// gwErrFail 网关提交错误的失败说明：网关 / Java 错误只取错误码与类别，message 是固定句式，不带原始响应体。
func gwErrFail(err error, temporary bool) taskevent.Fail {
	f := taskevent.FromError(err, "studio.gateway_submit")
	f.Class = "GATEWAY_SUBMIT"
	if temporary {
		f.Class = "TEMPORARY"
	}
	if f.Code == "ERROR" {
		f.Code = "SUBMIT_FAILED"
	}
	return f
}

// failSeq 提交失败的那次调用自己的序号（毫秒时间）：同一 attempt 下后来成功的 span_end 不会被失败行吞掉。
func failSeq() taskevent.SpanOpt {
	return taskevent.WithDedupeSuffix("failed-" + strconv.Itoa(nowSeq()))
}

// failClass / failCode 网关调用的失败分类与码（没有失败信息时用 GATEWAY）。
func failClass(c *aigateway.Call) string {
	if c != nil && c.Failure != nil && c.Failure.Class != "" {
		return c.Failure.Class
	}
	return "GATEWAY"
}

func failCode(c *aigateway.Call) string {
	if c != nil && c.Failure != nil && c.Failure.Code != "" {
		return c.Failure.Code
	}
	return "CALL_FAILED"
}
