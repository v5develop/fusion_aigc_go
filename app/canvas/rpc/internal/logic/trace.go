package logic

// 任务轨迹（afv_task_event）：把 canvas run 的完整生命周期写进与 Java 共用的事件表，管理端按创作任务号统一展示。
// 契约见 pkg/taskevent。task_id = taskcenter.Ref(TaskIDPrefix, run.ID)，即任务中心同步进 afv_creation_task 的那个编号
// （注意不是 run.TaskID：那是提交执行面 / 网关用的 NewLifeTaskID）。写入尽力而为，从不影响业务路径。
//
// step_path 一览：
//
//	go.canvas/queue                 受理落库 → 转运行中（或在排队 / 提交中终结）
//	go.canvas/freeze                冻结点数（gateway 后端）
//	go.canvas/dispatch              向后端提交（attempt=第几次）
//	go.canvas/gateway_call/<i>      第 i 个网关调用的创建（external_ref=调用号）+ gateway_call_linked
//	go.canvas/wait                  运行中等待结果（到终态 / 取消 / 超时）
//	go.canvas/timeout               超时判定（fail=timeout；延长 / 取消未受理=skipped，reason 说明）
//	go.canvas/settle                成功后确认扣费
//	go.canvas/refund                失败 / 取消的退款
//	go.canvas/rehost                产物转存素材库
//	go.canvas/deliver               置成功交付
//	go.canvas/cancel                取消请求（mark）

import (
	"context"
	"errors"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/taskevent"
)

const (
	stQueue    = "go.canvas/queue"
	stFreeze   = "go.canvas/freeze"
	stDispatch = "go.canvas/dispatch"
	stCall     = "go.canvas/gateway_call/"
	stWait     = "go.canvas/wait"
	stTimeout  = "go.canvas/timeout"
	stSettle   = "go.canvas/settle"
	stRefund   = "go.canvas/refund"
	stRehost   = "go.canvas/rehost"
	stDeliver  = "go.canvas/deliver"
	stCancel   = "go.canvas/cancel"
)

// traceIDOf run 主键 → 创作任务号。
func (c *core) traceIDOf(runPK int64) string {
	return taskcenter.Ref(c.s.Config.TaskIDPrefix, runPK)
}

func (c *core) evStart(ctx context.Context, run *model.NodeRun, step string, attempt int, attrs taskevent.Attrs, opts ...taskevent.SpanOpt) {
	if c.s.Events == nil || run == nil {
		return
	}
	c.s.Events.Start(ctx, c.traceIDOf(run.ID), step, attempt, attrs, opts...)
}

func (c *core) evEnd(ctx context.Context, run *model.NodeRun, step string, attempt int, status string, f taskevent.Fail, attrs taskevent.Attrs, opts ...taskevent.SpanOpt) {
	if c.s.Events == nil || run == nil {
		return
	}
	c.s.Events.Finish(ctx, c.traceIDOf(run.ID), step, attempt, status, f, attrs, opts...)
}

func (c *core) evMark(ctx context.Context, run *model.NodeRun, step, msg string, seq int, attrs taskevent.Attrs) {
	if c.s.Events == nil || run == nil {
		return
	}
	c.s.Events.Mark(ctx, c.traceIDOf(run.ID), step, msg, seq, attrs)
}

// runAttrs 各事件共用的基础属性（脱敏标量）。
func runAttrs(run *model.NodeRun, extra taskevent.Attrs) taskevent.Attrs {
	a := taskevent.Attrs{"kind": string(run.Kind), "backend": string(run.Backend)}
	if run.ModelGroup != "" {
		a["model_group"] = run.ModelGroup
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// failKind 失败码 → span_end.status。
func failStatus(code string) string {
	switch code {
	case "TIMEOUT", "SUBMIT_TIMEOUT":
		return taskevent.StatusTimeout
	case "CANCELED":
		return taskevent.StatusCancelled
	}
	return taskevent.StatusFailed
}

// traceEnded run 被 endRun 置终态后收尾仍开着的区间：from=提交中 / 排队 → queue；运行中 → wait。
func (c *core) traceEnded(ctx context.Context, run *model.NodeRun, from int8, code, msg string, refunded bool) {
	timeoutReasons.forget(run.ID)
	if c.s.Events == nil {
		return
	}
	step := stWait
	if from == model.RunSubmitting || from == model.RunQueued {
		step = stQueue
	}
	f := taskevent.Fail{Class: "LOCAL", Code: code, Check: "canvas.run_" + string(run.Kind), Message: msg}
	c.evEnd(ctx, run, step, 0, failStatus(code), f, runAttrs(run, taskevent.Attrs{"refunded": refunded}))
	if code == "TIMEOUT" && !(run.Backend == model.BackendGateway && from == model.RunRunning) {
		// gateway 运行中的超时由 applyGatewayTimeout 自己写 go.canvas/timeout（带判定依据）
		reason := "run_timeout"
		if step == stQueue {
			reason = "queue_timeout"
		}
		a := runAttrs(run, taskevent.Attrs{"reason": reason, "elapsed_ms": c.now().Sub(run.CreatedAt).Milliseconds()})
		c.evStart(ctx, run, stTimeout, 3, a)
		c.evEnd(ctx, run, stTimeout, 3, taskevent.StatusTimeout, f, a)
	}
	if code == "CANCELED" {
		c.evMark(ctx, run, stCancel, "取消", 0, taskevent.Attrs{"reason": code})
	}
}

// refundTrace 退款（Billing.Fail 无返回值，失败由 billing 自己记 ERROR）：写 go.canvas/refund 区间。
// 区间在 Billing.Fail 调用之后才收尾，status=ok 只表示「已请求退款」（result=requested），不保证账已退成。
func (c *core) refundTrace(ctx context.Context, run *model.NodeRun, reason string) {
	c.evStart(ctx, run, stRefund, 0, runAttrs(run, taskevent.Attrs{"reason": reason}))
	c.s.Billing.Fail(ctx, run.UserID, run.BillingRequestID.String, reason)
	c.evEnd(ctx, run, stRefund, 0, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, taskevent.Attrs{"reason": reason, "points": run.Points, "result": "requested"}))
}

// reasonCache 每个 run 最近一次写过的「超时延长」原因：原因没变就不再写事件（每个轮询 tick 都会再判一次）。
// 进程内、有界；run 终态时清掉（traceEnded / 成功收尾），超过上限整体清空（最坏多写几条，dedupe 兜底）。
type reasonCache struct {
	mu sync.Mutex
	m  map[int64]reasonState
}

type reasonState struct {
	reason string
	n      int // 第几次变化（进 dedupe：A→B→A 的第二个 A 也会记）
}

const reasonCacheMax = 10000

var timeoutReasons = &reasonCache{m: map[int64]reasonState{}}

// changed 记录并返回 reason 是否与上次不同；不同时 n 是第几次变化。
func (r *reasonCache) changed(runID int64, reason string) (n int, changed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.m[runID]
	if ok && st.reason == reason {
		return st.n, false
	}
	if len(r.m) >= reasonCacheMax {
		r.m = map[int64]reasonState{}
	}
	st = reasonState{reason: reason, n: st.n + 1}
	r.m[runID] = st
	return st.n, true
}

func (r *reasonCache) forget(runID int64) {
	r.mu.Lock()
	delete(r.m, runID)
	r.mu.Unlock()
}

// traceErrMsg 错误 → 事件 message：网关 / Java 业务错误只给码，其余本地错误给脱敏短文案（≤120 字符）。
func traceErrMsg(err error) string { return taskevent.FromError(err, "").Message }

// nowSeq 没有持久计数器的重试区间（交付 / 同步转存）的 attempt 序号：毫秒时间，同一次尝试的 start / end 共用，
// 不同次尝试互不相同，dedupe 不会把后一次吞掉。
func nowSeq() int { return int(time.Now().UnixMilli() % 1_000_000_000) }

// deliverEnd 置成功这一步的结局：CAS 出错 → failed（下轮重试）；没抢到（已取消 / 强制失败）→ skipped；抢到 → ok。
func (c *core) deliverEnd(ctx context.Context, run *model.NodeRun, seq int, ok bool, err error, outputs int) {
	switch {
	case err != nil:
		c.evEnd(ctx, run, stDeliver, seq, taskevent.StatusFailed, taskevent.Fail{Class: "LOCAL", Code: "DELIVER_WRITE", Check: "canvas.deliver", Message: traceErrMsg(err)}, runAttrs(run, nil))
	case !ok:
		c.evEnd(ctx, run, stDeliver, seq, taskevent.StatusSkipped, taskevent.Fail{Class: "LOCAL", Code: "STATE_CHANGED", Check: "canvas.deliver_cas", Message: "已被取消或强制失败，不交付"}, runAttrs(run, nil))
	default:
		c.evEnd(ctx, run, stDeliver, seq, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, taskevent.Attrs{"outputs": outputs}))
	}
}

// rehostEnd 同步转存的结局（errs 与待转存的产物一一对应）：全成功 ok；首个错误暂时性 → failed/TEMPORARY，否则 failed/PERMANENT。
func (c *core) rehostEnd(ctx context.Context, run *model.NodeRun, seq int, errs []error) {
	for _, err := range errs {
		if err == nil {
			continue
		}
		class := "PERMANENT"
		if isTemporary(err) {
			class = "TEMPORARY"
		}
		c.evEnd(ctx, run, stRehost, seq, taskevent.StatusFailed, taskevent.Fail{Class: class, Code: "REHOST_FAILED", Check: "canvas.rehost", Message: traceErrMsg(err)}, runAttrs(run, nil))
		return
	}
	c.evEnd(ctx, run, stRehost, seq, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, nil))
}

// jobsOf 待转存的产物个数（只用来写属性）。
func jobsOf(cands []model.Candidate, videoURL string) []struct{} {
	var out []struct{}
	for _, cd := range cands {
		if _, ok := pendingRef(cd.URL); ok {
			out = append(out, struct{}{})
		}
	}
	if _, ok := pendingRef(videoURL); ok {
		out = append(out, struct{}{})
	}
	return out
}

// gatewayErrCode 网关提交错误的失败码（没有网关错误码时用 SUBMIT_FAILED）。
func gatewayErrCode(err error) string {
	var ge *aigateway.Error
	if errors.As(err, &ge) && ge.Code != "" {
		return string(ge.Code)
	}
	return "SUBMIT_FAILED"
}

// dispatchFail 提交错误 → span_end 的失败说明。
func dispatchFail(err error) taskevent.Fail {
	f := taskevent.FromError(err, "canvas.dispatch")
	f.Class = "SUBMIT"
	if isTemporary(err) {
		f.Class = "TEMPORARY"
	}
	if f.Code == "ERROR" {
		f.Code = "SUBMIT_FAILED"
	}
	return f
}

// traceRunning 排队 → 运行中：收尾 dispatch / queue 区间，开 wait 区间。dn<=0 表示没有 dispatch 区间（Java 找回路径）。
func (c *core) traceRunning(ctx context.Context, run *model.NodeRun, dn, calls int) {
	if dn > 0 {
		c.evEnd(ctx, run, stDispatch, dn, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, taskevent.Attrs{"calls": calls, "attempt": dn}))
	}
	c.evEnd(ctx, run, stQueue, 0, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, taskevent.Attrs{"queued_ms": c.now().Sub(run.CreatedAt).Milliseconds()}))
	wa := taskevent.Attrs{"calls": calls}
	if run.Backend != model.BackendGateway && run.BackendRef.Valid {
		wa["backend_ref"] = run.BackendRef.String // Java 任务号 / studio 批次号
	}
	c.evStart(ctx, run, stWait, 0, runAttrs(run, wa))
}
