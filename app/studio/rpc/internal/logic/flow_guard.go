package logic

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件是 Poller 的兜底与计费善后：续期、提交中崩溃的退款、批次拉回失败的兜底、退款口径。

const (
	// maintainEvery maintain 的间隔。续期把预留单顺延到「现在 + billing.LeaseDuration（15 分钟）」，1 分钟一次足够。
	maintainEvery = time.Minute
	// renewConcurrency 续期并发（pkg/billing 没有批量接口，逐笔 UPDATE，限流）。
	renewConcurrency = 8
	// renewLimit 每轮最多续期多少笔。
	renewLimit = 5000
	// submittingStale 提交中超过这个时长视为提交进程已崩溃（提交链路冻结 + 善后各有 60s 超时）。
	submittingStale = 5 * time.Minute
	// billingTimeout 冻结 / 退款善后的独立超时（与调用方 ctx 的取消脱钩）。
	billingTimeout = 60 * time.Second
)

// detached 与调用方取消脱钩、带独立超时的 ctx：冻结与善后不能因为 RPC 超时 / 客户端断开 / 进程停止而中途放弃。
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), billingTimeout)
}

// refundable 这个子任务失败时是否要退款：有计费请求号且 Billing.Mode=on。不看 billing_status 镜像
// （镜像写失败时不能因此漏退；没冻结成功的单子 Fail 是空操作）。
func refundable(mode string, t *model.Task) bool {
	return mode == billing.ModeOn && t.BillingRequestID.Valid && t.BillingRequestID.String != ""
}

// 预留单状态（pkg/billing 内部常量的字面值）。
const (
	resvConfirmed         = "CONFIRMED"
	resvPartiallyRefunded = "PARTIALLY_REFUNDED"
)

// refundTask 退回一个子任务的点数：先按 HELD 退（Fail）；之后若预留单已是 CONFIRMED（已扣费，例如结算后才被改失败），
// 走已确认退款（RefundConfirmed）。调用方须先 CAS 抢到失败状态，保证只退一次。
func (f *flow) refundTask(ctx context.Context, userID int64, requestID, reason string) {
	ctx, cancel := detached(ctx)
	defer cancel()
	f.bill.Fail(ctx, userID, requestID, reason)
	st, err := f.bill.Status(ctx, userID, requestID)
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 查预留单 %s 状态失败（未核对是否已扣费）: %v", requestID, err)
		return
	}
	if st != resvConfirmed && st != resvPartiallyRefunded {
		return
	}
	n, err := f.bill.RefundConfirmed(ctx, userID, requestID, reason)
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 预留单 %s 已扣费，退回失败（需人工处理）: %v", requestID, err)
		return
	}
	logx.WithContext(ctx).Infow("studio refund confirmed reservation", logx.Field("billing_request_id", requestID), logx.Field("points", n))
}

// maintain 每 maintainEvery 做一次：续期、提交中兜底、终态批次的排队子任务兜底。
func (f *flow) maintain(ctx context.Context) {
	now := f.now()
	if !f.maintAt.IsZero() && now.Sub(f.maintAt) < maintainEvery {
		return
	}
	f.maintAt = now
	f.renewHeld(ctx)
	f.reapSubmitting(ctx)
	f.rescueStrayQueued(ctx)
	f.reconcileBilling(ctx)
}

const (
	// reconcileLimit 每轮最多核对多少笔「终态却仍是已冻结」的子任务。
	reconcileLimit = 500
	// 预留单的其它终态：过期 / 取消释放（UNFREEZE）。pkg/billing 的常量未导出，这里用字面值。
	resvRefunded = "REFUNDED"
	resvExpired  = "EXPIRED"
)

// billingMirror 预留单状态 → 子任务计费镜像。ok=false 表示不动（仍 HELD、无单或未知状态）。
//
//	CONFIRMED / PARTIALLY_REFUNDED → 已结算；REFUNDED / EXPIRED → 已退回。
func billingMirror(resvStatus string) (to int8, ok bool) {
	switch resvStatus {
	case resvConfirmed, resvPartiallyRefunded:
		return model.BillingSettled, true
	case resvRefunded, resvExpired:
		return model.BillingRefunded, true
	}
	return 0, false
}

// reconcileBilling 终态任务的计费镜像对账：结果未知（UNKNOWN）的任务置失败后预留单不退款、留给回收器到期释放，
// 释放时没人回写 billing_status，后台会一直显示「冻结」。这里按预留单的真实状态回写，仍 HELD 的不动（回收器还没到期）。
// 只用 CAS（billing_status=已冻结）写，不会覆盖并发路径写入的结果。
func (f *flow) reconcileBilling(ctx context.Context) {
	if f.bill == nil || f.bill.Mode() != billing.ModeOn {
		return
	}
	log := logx.WithContext(ctx)
	list, err := f.repo.Task.ListFinalHeld(ctx, reconcileLimit)
	if err != nil {
		log.Errorf("studio: 列待对账计费镜像的子任务失败: %v", err)
		return
	}
	for _, t := range list {
		st, err := f.bill.Status(ctx, t.UserID, t.BillingRequestID.String)
		if err != nil {
			log.Errorf("studio: 子任务 %d 查预留单状态失败: %v", t.ID, err)
			continue
		}
		to, ok := billingMirror(st)
		if !ok {
			continue
		}
		done, err := f.repo.Task.CASBillingStatus(ctx, t.ID, model.BillingHeld, to)
		if err != nil {
			log.Errorf("studio: 子任务 %d 回写计费镜像失败: %v", t.ID, err)
			continue
		}
		if done {
			log.Infow("studio billing mirror reconciled", logx.Field("task_id", t.ID),
				logx.Field("billing_request_id", t.BillingRequestID.String), logx.Field("reservation", st), logx.Field("billing_status", to))
		}
	}
}

// renewHeld 未终态批次下所有已冻结的在途子任务（排队 / 生成中 / 交付中，含编排阶段）批量续期。
// 排队很久（如 81 张排队 > 30 分钟）的预留单不会过期被回收，结算时也就不会 ErrGone。
func (f *flow) renewHeld(ctx context.Context) {
	if f.bill == nil || f.bill.Mode() != billing.ModeOn {
		return
	}
	log := logx.WithContext(ctx)
	list, err := f.repo.Task.ListHeldActive(ctx, renewLimit)
	if err != nil {
		log.Errorf("studio: 列待续期子任务失败: %v", err)
		return
	}
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, renewConcurrency)
	)
	for _, t := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(t *model.Task) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := f.bill.Renew(ctx, t.UserID, t.BillingRequestID.String); err != nil {
				log.Errorf("studio: 子任务 %d 续期失败: %v", t.ID, err)
			}
		}(t)
	}
	wg.Wait()
}

// reapSubmitting 提交中超过 submittingStale 的批次（提交进程在冻结途中崩溃）：先 CAS 批次 提交中→失败（与提交链路抢），
// 抢到后把排队的子任务逐个 CAS → 失败并退款（有计费请求号的都 Fail 一次，没冻结成功的是空操作）。
func (f *flow) reapSubmitting(ctx context.Context) {
	log := logx.WithContext(ctx)
	list, err := f.repo.Batch.ListStaleSubmitting(ctx, f.now().Add(-submittingStale).UnixMilli(), f.batchSize())
	if err != nil {
		log.Errorf("studio: 列提交中批次失败: %v", err)
		return
	}
	for _, b := range list {
		ok, err := f.repo.Batch.UpdateStatus(ctx, b.ID, model.BatchSubmitting, model.BatchFailed,
			map[string]any{"error_reason": reasonSubmitAborted, "task_failed": b.TaskTotal})
		if err != nil {
			log.Errorf("studio: 批次 %d 提交中→失败出错: %v", b.ID, err)
			continue
		}
		if !ok {
			continue
		}
		tasks, err := f.repo.Task.ListByBatch(ctx, b.ID)
		if err != nil {
			log.Errorf("studio: 批次 %d 列子任务失败: %v", b.ID, err)
			continue
		}
		mode := f.bill.Mode()
		for _, t := range tasks {
			if t.Status != model.TaskQueued {
				continue
			}
			refund := refundable(mode, t)
			extra := map[string]any{"error_reason": reasonSubmitAborted}
			if refund {
				extra["billing_status"] = model.BillingRefunded
			}
			ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, model.TaskFailed, extra)
			if err != nil || !ok {
				continue
			}
			if refund {
				f.refundTask(ctx, b.UserID, t.BillingRequestID.String, reasonSubmitAborted)
			}
		}
		log.Errorw("studio submitting batch reaped", logx.Field("batch_id", b.ID), logx.Field("task_total", b.TaskTotal))
		f.stepLog(ctx, b.ID, model.StepLog{Step: "submit_aborted", TaskTotal: int(b.TaskTotal), Detail: reasonSubmitAborted})
	}
}

// rescueStrayQueued 已终态却还有排队子任务的批次（后台重做时批次拉回失败）：
// 冻结有效的（Mode=on 时镜像为已冻结；off / shadow 不需要冻结）拉回进行中，prompt 已就绪 → 生成中，否则 → 排队重新编排；
// 冻结无效的（已退回 / 从未冻结）不能再生成，CAS → 失败（Fail 一次兜底，空操作无害）。
func (f *flow) rescueStrayQueued(ctx context.Context) {
	log := logx.WithContext(ctx)
	ids, err := f.repo.Task.BatchesWithStrayQueued(ctx, f.batchSize())
	if err != nil {
		log.Errorf("studio: 列终态批次的排队子任务失败: %v", err)
		return
	}
	mode := f.bill.Mode()
	for _, id := range ids {
		b, err := f.repo.Batch.GetByID(ctx, id, 0)
		if err != nil || b == nil || !model.BatchFinal(b.Status) {
			continue
		}
		tasks, err := f.repo.Task.ListByBatch(ctx, b.ID)
		if err != nil {
			continue
		}
		to, revive, failed := model.BatchGenerating, 0, 0
		for _, t := range tasks {
			if t.Status != model.TaskQueued || t.AdminLocked != 0 {
				continue
			}
			if mode != billing.ModeOn || t.BillingStatus == model.BillingHeld {
				revive++
				if !t.PromptText.Valid || t.PromptText.String == "" {
					to = model.BatchQueued
				}
				continue
			}
			ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, model.TaskFailed,
				map[string]any{"error_reason": reasonSubmitAborted})
			if err != nil || !ok {
				continue
			}
			failed++
			if refundable(mode, t) {
				f.refundTask(ctx, b.UserID, t.BillingRequestID.String, reasonSubmitAborted)
			}
		}
		if failed > 0 {
			if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 0, failed); err != nil {
				log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
			}
		}
		if revive == 0 {
			continue
		}
		ok, err := f.repo.Batch.UpdateStatus(ctx, b.ID, b.Status, to, map[string]any{"error_reason": nil})
		if err != nil || !ok {
			log.Errorf("studio: 批次 %d 兜底拉回进行中失败: ok=%v err=%v", b.ID, ok, err)
			continue
		}
		log.Infow("studio stray queued batch revived", logx.Field("batch_id", b.ID), logx.Field("to", batchStatusName(to)),
			logx.Field("tasks", revive))
		f.stepLog(ctx, b.ID, model.StepLog{Step: "revived", TaskTotal: revive, Detail: batchStatusName(to)})
	}
}
