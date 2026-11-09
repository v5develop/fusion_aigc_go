package logic

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// transferGrace 已出图但转存暂时失败时，超过生图时限后再宽限多久才放弃（失败退款）。
const transferGrace = 10 * time.Minute

// defaultDeliverGrace 交付中（已转存、待结算）最多停留多久（Timeouts.DeliverGrace 未配时）。超期仍结算不了就按预留单状态出场。
const defaultDeliverGrace = 30 * time.Minute

// deliveryOverdue 交付中已超过宽限期。进交付中时 TransitStatus 写 updated_at=NOW(3)，之后交付中不再有业务写入
// （轮询的租约读写都保留 updated_at），所以 updated_at 就是进交付中的时刻。
func deliveryOverdue(t *model.Task, now time.Time, grace time.Duration) bool {
	return t.Status == model.TaskDelivering && now.Sub(t.UpdatedAt) > grace
}

// pollTask 一个 生成中 子任务走一步：补提交 → 长轮询 → 终态处理；交付中 的续做交付。续期在 maintain 里统一做。
func (f *flow) pollTask(ctx context.Context, id int64) {
	log := logx.WithContext(ctx)
	t, err := f.repo.Task.GetByID(ctx, id)
	if err != nil || t == nil || t.AdminLocked != 0 || (t.Status != model.TaskRunning && t.Status != model.TaskDelivering) {
		return
	}
	b, err := f.repo.Batch.GetByID(ctx, t.BatchID, 0)
	if err != nil || b == nil {
		return
	}
	started := t.UpdatedAt
	if t.StartedAt.Valid {
		started = t.StartedAt.Time
	}
	if t.Status == model.TaskDelivering { // 结算前进程退出 / 结算暂时失败：续做交付
		f.resumeDeliver(ctx, t, b, started)
		return
	}
	timedOut := f.now().Sub(started) > f.generateTimeout()

	// 1. 没有调用号：上次提交失败 / 刚派发，按幂等键（重）提交，不会重复扣费
	if !t.GatewayCallID.Valid || t.GatewayCallID.String == "" {
		call, err := f.submit(ctx, t, b, false)
		if timedOut {
			// 超时：同幂等键取回调用号（可能上次其实提交成功了）再取消，避免网关侧继续跑、继续计费
			if err != nil {
				log.Errorf("studio: 子任务 %d 超时且取不回调用号: %v", t.ID, err)
				f.failTask(ctx, t, b, reasonTimeout, true, "")
				return
			}
			f.cancelOnTimeout(ctx, t, b, call, started)
			return
		}
		if err != nil {
			if pipeline.RetryableSubmitErr(err) {
				log.Infof("studio: 子任务 %d 提交 gateway 暂时失败，下一轮重提: %v", t.ID, err)
				return
			}
			log.Errorf("studio: 子任务 %d 提交 gateway 失败: %v", t.ID, err)
			f.failTask(ctx, t, b, reasonSubmitFailed, true, "")
			return
		}
		t.GatewayCallID.String, t.GatewayCallID.Valid = call.ID, true
		t.GatewayStatus.String = call.Status
	}

	// 2. 长轮询
	call, err := f.gw.Get(ctx, t.GatewayCallID.String, f.longPollWait())
	if err != nil {
		if errors.Is(err, aigateway.ErrNotFound) {
			f.failTask(ctx, t, b, reasonLost, true, "")
			return
		}
		if !errors.Is(err, context.Canceled) {
			log.Errorf("studio: 子任务 %d 查询 gateway 失败: %v", t.ID, err)
		}
		return
	}
	if call.Status != t.GatewayStatus.String {
		_ = f.repo.Task.SetGateway(ctx, t.ID, "", call.Status)
	}
	if !call.IsTerminal() {
		if timedOut {
			f.cancelOnTimeout(ctx, t, b, call, started)
		}
		return
	}
	f.onTerminal(ctx, t, b, call, started)
}

// cancelOnTimeout 超时：取消 gateway 调用（尽力而为）。取消时已出图（返回 SUCCEEDED）照常交付，其它终态按终态处理，
// 取消失败或仍未终态则超时失败退款。
func (f *flow) cancelOnTimeout(ctx context.Context, t *model.Task, b *model.Batch, call *aigateway.Call, started time.Time) {
	log := logx.WithContext(ctx)
	if call.IsTerminal() {
		f.onTerminal(ctx, t, b, call, started)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cc, err := f.gw.Cancel(cctx, call.ID)
	cancel()
	if err != nil {
		log.Errorf("studio: 子任务 %d 超时取消 gateway 调用失败（尽力而为）: %v", t.ID, err)
	}
	if err == nil && cc != nil && cc.Succeeded() {
		log.Infof("studio: 子任务 %d 超时取消时已出图，照常交付", t.ID)
		if outs, _ := pipeline.ParseOutputs(cc); len(outs) == 0 {
			// 取消接口的返回可能不带 result：再 GET 一次拿完整结果，取不到才按空结果处理
			gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
			gc, gerr := f.gw.Get(gctx, call.ID, 0)
			gcancel()
			if gerr == nil && gc != nil && gc.Succeeded() {
				cc = gc
			} else {
				log.Errorf("studio: 子任务 %d 取消返回成功但没有结果，补查失败: %v", t.ID, gerr)
			}
		}
		f.deliver(ctx, t, b, cc, started)
		return
	}
	status := call.Status
	if err == nil && cc != nil {
		status = cc.Status
	}
	f.failTask(ctx, t, b, reasonTimeout, true, status)
}

// onTerminal 终态处理。
func (f *flow) onTerminal(ctx context.Context, t *model.Task, b *model.Batch, call *aigateway.Call, started time.Time) {
	log := logx.WithContext(ctx)
	act, delay := decideTerminal(call)
	switch act {
	case actDeliver:
		f.deliver(ctx, t, b, call, started)
	case actRetry:
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		nc, err := f.submit(ctx, t, b, true)
		if err != nil {
			if pipeline.RetryableSubmitErr(err) {
				return // 下一轮查到的仍是 FAILED，会再走这里
			}
			log.Errorf("studio: 子任务 %d retry_failed 失败: %v", t.ID, err)
			f.failTask(ctx, t, b, failureReason(call), true, call.Status)
			return
		}
		if nc.IsTerminal() && nc.RunNo <= call.RunNo { // 网关没有重开
			f.failTask(ctx, t, b, failureReason(nc), true, nc.Status)
			return
		}
		// attempt+1；进 生成中 会重置 started_at，新一轮有自己的时限
		ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskRunning, model.TaskRunning,
			map[string]any{"attempt": t.Attempt + 1, "gateway_status": nc.Status})
		if err != nil {
			log.Errorf("studio: 子任务 %d 记录重试失败: %v", t.ID, err)
			return
		}
		if !ok { // 等待重开期间被强制失败 / 取消：新开的一轮不能再跑
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if _, err := f.gw.Cancel(cctx, nc.ID); err != nil {
				log.Errorf("studio: 子任务 %d 状态已变，取消重开的调用失败（尽力而为）: %v", t.ID, err)
			}
			cancel()
			return
		}
		log.Infow("studio generate retry", logx.Field("task_id", t.TaskID.String), logx.Field("run_no", nc.RunNo),
			logx.Field("class", call.Failure.Class))
	case actUnknown:
		// 供应商侧结果未知，可能已扣费：标失败但不主动退款，预留单不再续期，到期由回收器（billing.Reclaimer）自动退回
		log.Errorw("studio generate outcome unknown, reservation left to reclaimer",
			logx.Field("task_id", t.TaskID.String), logx.Field("call_id", call.ID), logx.Field("batch_id", b.ID),
			logx.Field("money_at_risk", call.MoneyAtRisk), logx.Field("billing_request_id", t.BillingRequestID.String))
		f.failTask(ctx, t, b, reasonUnknown, false, call.Status)
	default:
		if call.Failure != nil {
			log.Infow("studio generate failed", logx.Field("task_id", t.TaskID.String), logx.Field("call_id", call.ID),
				logx.Field("class", call.Failure.Class), logx.Field("code", call.Failure.Code))
		}
		f.failTask(ctx, t, b, failureReason(call), true, call.Status)
	}
}

// resumeDeliver 交付中 的子任务续做。
//   - 成品已转存（result_url 已写，正常进交付中都会写）：直接结算，不依赖 gateway（只尽力 GET 一次补成本，取不到记 0）；
//   - result_url 没写（异常数据）：按调用结果重新交付；
//   - 超过 deliverGrace 仍结算不了：告警并按预留单状态出场（finishDelivery / failTask），保证交付中有出口。
func (f *flow) resumeDeliver(ctx context.Context, t *model.Task, b *model.Batch, started time.Time) {
	log := logx.WithContext(ctx)
	overdue := deliveryOverdue(t, f.now(), f.deliverGrace())
	hasCall := t.GatewayCallID.Valid && t.GatewayCallID.String != ""
	if t.ResultURL.Valid && t.ResultURL.String != "" {
		var call *aigateway.Call
		if f.gwOn && hasCall {
			gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			c, err := f.gw.Get(gctx, t.GatewayCallID.String, 0)
			cancel()
			if err == nil && c != nil && c.Succeeded() {
				call = c
			} else {
				log.Infof("studio: 交付中子任务 %d 补查 gateway 未果（不影响结算）: %v", t.ID, err)
			}
		}
		res := mediasvc.Resource{RID: t.ResultRid.String, URL: t.ResultURL.String, Width: int(t.ResultWidth), Height: int(t.ResultHeight)}
		f.finishDelivery(ctx, t, b, call, res, started, overdue)
		return
	}
	var call *aigateway.Call
	var err error
	if hasCall {
		call, err = f.gw.Get(ctx, t.GatewayCallID.String, 0)
	}
	if !hasCall || err != nil || call == nil || !call.Succeeded() {
		status := ""
		if call != nil {
			status = call.Status
		}
		log.Errorf("studio: 交付中子任务 %d 没有成品地址，且取不到成功的调用结果（%s）: %v", t.ID, status, err)
		if overdue {
			log.Errorw("studio delivering overdue, failing task", logx.Field("task_id", t.TaskID.String),
				logx.Field("batch_id", b.ID), logx.Field("billing_request_id", t.BillingRequestID.String))
			f.failTask(ctx, t, b, reasonDeliverTimeout, true, status)
		}
		return
	}
	f.deliverCall(ctx, t, b, call, started, overdue)
}

// deliver 成功：转存素材库桶 → CAS 生成中→交付中（抢交付锁，同时写成品）→ 结算（返回 nil 才交付）
// → CAS 交付中→成功（写成本）→ 入素材库。交付中的任务不会被强制失败 / 超时失败改掉，结算后不会出现「已扣费却失败」。
func (f *flow) deliver(ctx context.Context, t *model.Task, b *model.Batch, call *aigateway.Call, started time.Time) {
	f.deliverCall(ctx, t, b, call, started, false)
}

func (f *flow) deliverCall(ctx context.Context, t *model.Task, b *model.Batch, call *aigateway.Call, started time.Time, overdue bool) {
	log := logx.WithContext(ctx)
	outs, _ := pipeline.ParseOutputs(call)
	if len(outs) == 0 || outs[0].RID == "" {
		f.failTask(ctx, t, b, reasonEmptyResult, true, call.Status)
		return
	}
	var res mediasvc.Resource
	if t.Status == model.TaskDelivering && t.ResultURL.Valid && t.ResultURL.String != "" {
		res = mediasvc.Resource{RID: t.ResultRid.String, URL: t.ResultURL.String, Width: int(t.ResultWidth), Height: int(t.ResultHeight)}
	} else if f.watermarkDelivery(ctx, b.UserID) {
		// 免费档：原件就是网关产物（media_purpose=intermediate，私有桶），不转存公有桶；
		// 结果与素材库写水印视图 mrw://，读时只能签成带水印参数的地址（见 free_watermark.go）
		res = mediasvc.Resource{RID: outs[0].RID, URL: watermarkRef(outs[0].RID, firstMediaMime(call))}
		log.Infow("studio deliver: free tier, watermark view (check=studio_free_watermark.deliver)",
			logx.Field("task_id", t.TaskID.String), logx.Field("user_id", b.UserID))
	} else {
		giveUp := overdue || f.now().Sub(started) > f.generateTimeout()+transferGrace
		if f.media == nil {
			log.Errorf("studio: 子任务 %d 已出图，但媒体服务未配置，无法转存", t.ID)
			if giveUp {
				f.failTask(ctx, t, b, reasonTransfer, true, call.Status)
			}
			return
		}
		var err error
		res, err = f.media.CopyToLibrary(ctx, outs[0].RID, mediasvc.PurposeAssetLibrary, "")
		if err == nil && res.URL == "" {
			err = &mediasvc.Error{Status: 422, Code: "no_public_url", Message: "素材库桶没有公开地址"}
		}
		if err != nil {
			var me *mediasvc.Error
			if (errors.As(err, &me) && me.Permanent()) || giveUp {
				log.Errorf("studio: 子任务 %d 转存失败，放弃: %v", t.ID, err)
				f.failTask(ctx, t, b, reasonTransfer, true, call.Status)
				return
			}
			log.Errorf("studio: 子任务 %d 转存失败，下一轮重试: %v", t.ID, err)
			return
		}
	}
	if res.Width == 0 || res.Height == 0 {
		res.Width, res.Height = outs[0].W, outs[0].H
	}
	// 交付锁：抢到 生成中→交付中 才结算；没抢到说明已被强制失败 / 超时失败 / 别处处理
	if t.Status == model.TaskRunning {
		ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskRunning, model.TaskDelivering, map[string]any{
			"result_rid": res.RID, "result_url": res.URL, "result_width": res.Width, "result_height": res.Height, "gateway_status": call.Status,
		})
		if err != nil {
			log.Errorf("studio: 子任务 %d 进交付中失败: %v", t.ID, err)
			return
		}
		if !ok {
			return
		}
		t.Status = model.TaskDelivering
		overdue = false
	}
	if t.Status != model.TaskDelivering {
		return
	}
	f.finishDelivery(ctx, t, b, call, res, started, overdue)
}

// finishDelivery 交付中 → 结算 → 成功。call 可为 nil（续做交付时 gateway 取不到）：成本记 0 并告警。
//
// 结算结果：
//   - nil：已确认扣费（或计费未开启），CAS 交付中→成功；
//   - ErrGone：再查预留单真实状态。CONFIRMED / PARTIALLY_REFUNDED = 别的实例 / 上一轮已结算（并发交付），按成功交付；
//     已释放（过期 / 退回）= 不交付，置失败不退款；仍 HELD（查询与加锁之间的瞬时状态）下一轮再结算；
//   - 其它错误：留在交付中下一轮重试；超过 deliverGrace 仍失败则按预留单状态出场：已扣费 → 成功交付，
//     仍 HELD → 置失败并退款（Fail），已释放 → 置失败（交付超时，点数已退回）。
func (f *flow) finishDelivery(ctx context.Context, t *model.Task, b *model.Batch, call *aigateway.Call, res mediasvc.Resource, started time.Time, overdue bool) {
	log := logx.WithContext(ctx)
	gwStatus := ""
	if call != nil {
		gwStatus = call.Status
	}
	reqID := t.BillingRequestID.String
	sctx, cancel := detached(ctx)
	err := f.bill.Settle(sctx, b.UserID, reqID, 1)
	cancel()
	if err != nil {
		gone := errors.Is(err, billing.ErrGone)
		if !gone && !overdue {
			log.Errorf("studio: 子任务 %d 结算失败，下一轮重试: %v", t.ID, err)
			return
		}
		qctx, qcancel := detached(ctx)
		st, serr := f.bill.Status(qctx, b.UserID, reqID)
		qcancel()
		if serr != nil {
			log.Errorw("studio deliver: settle failed and reservation status unknown, retry next round",
				logx.Field("task_id", t.TaskID.String), logx.Field("billing_request_id", reqID),
				logx.Field("overdue", overdue), logx.Field("settle_err", err.Error()), logx.Field("status_err", serr.Error()))
			return
		}
		if overdue {
			log.Errorw("studio delivering overdue: settle keeps failing", logx.Field("task_id", t.TaskID.String),
				logx.Field("batch_id", b.ID), logx.Field("billing_request_id", reqID), logx.Field("reservation", st),
				logx.Field("settle_err", err.Error()))
		}
		switch {
		case st == resvConfirmed || st == resvPartiallyRefunded:
			log.Infow("studio deliver: reservation already settled elsewhere, delivering", logx.Field("task_id", t.TaskID.String),
				logx.Field("billing_request_id", reqID), logx.Field("reservation", st))
		case st == resvHeld:
			if !overdue {
				log.Errorf("studio: 子任务 %d 结算返回失效但预留单仍冻结，下一轮重试: %v", t.ID, err)
				return
			}
			f.failTask(ctx, t, b, reasonDeliverTimeout, true, gwStatus)
			return
		default: // 已过期 / 已退回 / 没有预留单：点数已不在冻结里，不交付
			reason := billing.DeliverBlockedReason
			if overdue {
				reason = reasonDeliverTimeout
			}
			log.Errorf("studio: 子任务 %d 预留已失效（%s），不交付: %v", t.ID, st, err)
			f.failTask(ctx, t, b, reason, false, gwStatus)
			return
		}
	}
	extra := map[string]any{
		"result_rid": res.RID, "result_url": res.URL, "result_width": res.Width, "result_height": res.Height, "error_reason": nil,
	}
	if call != nil {
		extra["gateway_status"] = call.Status
	}
	var cost *aigateway.Cost
	if call != nil {
		cost = call.Cost
	}
	c, costOK := costCNYText(cost)
	if !costOK {
		log.Errorf("studio: warning 子任务 %d 拿不到 gateway 调用的 amount_cny（调用 %s），成本记 0", t.ID, t.GatewayCallID.String)
	}
	extra["cost_cny"] = c
	if f.bill.Mode() == billing.ModeOn {
		extra["billing_status"] = model.BillingSettled
	}
	// 先入素材库再置成功：批次收敛按子任务状态计数，置成功之后才写资产会出现「批次已终态、资产行还没有」的窗口，
	// 前端在这个窗口里点「存为资产」拿到 asset_id=0。入库失败只记日志不拦交付（saveAsset 内部吞错）；
	// 置成功失败留在交付中下一轮续做时，saveAsset 按 source_task_id 查重，不会重复写。
	t.ResultURL.String = res.URL
	f.saveAsset(ctx, t, b, res.URL)
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskDelivering, model.TaskSucceeded, extra)
	if err != nil {
		log.Errorf("studio: 子任务 %d 置成功失败（留在交付中，下一轮续做）: %v", t.ID, err)
		return
	}
	if !ok {
		return
	}
	if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 1, 0); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
	// 服装蒙版（校色只作用于服装）：成功之后异步补，不拖慢、不影响交付
	f.attachMaskAsync(t, b.UserID, res.URL, tryonMaskParts(t.Kind, b.Payload))
	_, costCNY := pipeline.ParseOutputs(call)
	f.logStep(ctx, b.ID, pipeline.StepLog{
		JobID: t.TaskID.String, Step: pipeline.StepGenerate, Ms: f.now().Sub(started).Milliseconds(),
		Images: inputCount(t), CostCNY: costCNY,
	})
	f.converge(ctx, b.ID)
}

func inputCount(t *model.Task) int {
	var list []model.InputImage
	_ = json.Unmarshal(t.InputImages, &list)
	return len(list)
}

// failTask 子任务 → 失败（CAS，从当前状态），refund=true 时退款（只在 CAS 成功后退，保证一次）。
// 是否退款按 billing_request_id 非空且 Billing.Mode=on；预留单若已 CONFIRMED 走已确认退款（refundTask）。
// 从 交付中 失败（预留已失效 / 放弃转存）时清掉已写的成品地址。
func (f *flow) failTask(ctx context.Context, t *model.Task, b *model.Batch, reason string, refund bool, gwStatus string) {
	log := logx.WithContext(ctx)
	extra := map[string]any{"error_reason": reason}
	if gwStatus != "" {
		extra["gateway_status"] = gwStatus
	}
	if t.Status == model.TaskDelivering {
		extra["result_rid"], extra["result_url"] = nil, nil
	}
	refund = refund && refundable(f.bill.Mode(), t)
	if refund {
		extra["billing_status"] = model.BillingRefunded
	}
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, t.Status, model.TaskFailed, extra)
	if err != nil {
		log.Errorf("studio: 子任务 %d 置失败出错: %v", t.ID, err)
		return
	}
	if !ok {
		return
	}
	if refund {
		f.refundTask(ctx, b.UserID, t.BillingRequestID.String, reason)
	}
	if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 0, 1); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
	f.logStep(ctx, b.ID, pipeline.StepLog{JobID: t.TaskID.String, Step: pipeline.StepGenerate, Error: reason})
	f.converge(ctx, b.ID)
}

// forceFail 运营强制失败一个在途子任务（admin_force_fail_logic.go）：只从 排队 / 生成中 CAS → 失败（交付中拒绝）。
// 有调用号的先尽力 cancel：取消时已出图（SUCCEEDED）则拒绝，交给 Poller 照常交付；CAS 成功后退款、收敛批次。
func (f *flow) forceFail(ctx context.Context, t *model.Task, b *model.Batch) error {
	log := logx.WithContext(ctx)
	if b.Status == model.BatchSubmitting {
		return xerr.New(xerr.Conflict, "批次正在提交，请稍后再试")
	}
	switch t.Status {
	case model.TaskQueued, model.TaskRunning:
	case model.TaskDelivering:
		return xerr.New(xerr.Conflict, reasonDelivering)
	default:
		return xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
	}
	if t.Status == model.TaskRunning && t.GatewayCallID.Valid && t.GatewayCallID.String != "" && f.gwOn {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cc, err := f.gw.Cancel(cctx, t.GatewayCallID.String)
		cancel()
		if err != nil {
			log.Errorf("studio: 强制失败时取消 gateway 调用 %s 失败（尽力而为）: %v", t.GatewayCallID.String, err)
		} else if cc != nil && cc.Succeeded() {
			return xerr.New(xerr.Conflict, "任务已出图，正在交付，不能强制失败")
		}
	}
	extra := map[string]any{"admin_locked": 1, "error_reason": reasonForceFail}
	refund := refundable(f.bill.Mode(), t)
	if refund {
		extra["billing_status"] = model.BillingRefunded
	}
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, t.Status, model.TaskFailed, extra)
	if err != nil {
		return err
	}
	if !ok {
		return xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
	}
	if refund {
		f.refundTask(ctx, b.UserID, t.BillingRequestID.String, reasonForceFail)
	}
	if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 0, 1); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
	f.stepLog(ctx, b.ID, model.StepLog{Step: "admin_force_fail", Detail: t.TaskID.String})
	f.converge(ctx, b.ID)
	return nil
}

// submit 用子任务里存好的 prompt_text / input_images 重建请求并提交（幂等键见 idemKey）。
// retryFailed=true 用于 retry_safe 的 FAILED 记录重开新一轮。
func (f *flow) submit(ctx context.Context, t *model.Task, b *model.Batch, retryFailed bool) (*aigateway.Call, error) {
	req, err := buildTaskRequest(t, b)
	if err != nil {
		return nil, err
	}
	// 网关侧时限与费用上限：deadline_at 不参与 request_hash（每次重提可以不同），max_cost 参与（配置不变即一致）。
	// Go 侧超时（Timeouts.Generate）先到，到点主动 Cancel；deadline_at 多留 60s 只作兜底。
	deadline := f.now().Add(f.generateTimeout() + 60*time.Second).UTC()
	req.Constraints = &aigateway.Constraints{DeadlineAt: &deadline}
	if mc := strings.TrimSpace(f.cfg.AiGateway.MaxCostCNY); mc != "" {
		req.Limits = &aigateway.Limits{MaxCost: mc}
	}
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	call, err := f.gw.Submit(sctx, req, aigateway.SubmitOptions{IdempotencyKey: idemKey(b.ID, t), RetryFailed: retryFailed})
	if err != nil {
		return nil, err
	}
	if err := f.repo.Task.SetGateway(ctx, t.ID, call.ID, call.Status); err != nil {
		logx.WithContext(ctx).Errorf("studio: 子任务 %d 写 gateway 调用号失败: %v", t.ID, err)
	}
	return call, nil
}

// buildTaskRequest 子任务 → gateway 请求。prompt_text 已含首行图序（WithImageOrderLine 会先剥再加，结果不变），
// 生图参数取 prompt_meta.gen（编排时定下的比例 / 分辨率 / 质量），参考图取 input_images（已按上限截断，重建时不再截），
// 所以重启 / 重提得到与首次相同的请求体，同幂等键不会 409。
func buildTaskRequest(t *model.Task, b *model.Batch) (aigateway.CallRequest, error) {
	p, err := parsePayload(b.Payload)
	if err != nil {
		return aigateway.CallRequest{}, err
	}
	var inputs []model.InputImage
	if len(t.InputImages) > 0 {
		if err := json.Unmarshal(t.InputImages, &inputs); err != nil {
			return aigateway.CallRequest{}, fmt.Errorf("input_images 无法解析: %w", err)
		}
	}
	gp := genParams{AspectRatio: p.AspectRatio, Resolution: p.Resolution}
	if len(t.PromptMeta) > 0 {
		var meta struct {
			Gen *genParams `json:"gen"`
		}
		if json.Unmarshal(t.PromptMeta, &meta) == nil && meta.Gen != nil {
			gp = *meta.Gen
		}
	}
	refs := make([]aigateway.MediaRef, 0, len(inputs))
	for _, im := range inputs {
		refs = append(refs, aigateway.MediaRef{URI: im.URL, Sha256: im.Sha256})
	}
	opt := pipeline.GenOptions{Quality: gp.Quality, MaxRefImages: len(refs)}
	labels := map[string]string{"batch": fmt.Sprint(b.ID), "task": t.TaskID.String, "kind": string(t.Kind)}
	prompt := t.PromptText.String
	switch t.Kind {
	case model.KindTryon:
		req, _, err := pipeline.BuildImageEditRequestRefs(pipeline.KindTryon, t.ModelGroup, prompt, refs, gp.Resolution, gp.AspectRatio, labels, opt)
		return req, err
	case model.KindPose:
		req, _, err := pipeline.BuildImageEditRequestRefs(pipeline.KindPose, t.ModelGroup, prompt, refs, gp.Resolution, gp.AspectRatio, labels, opt)
		return req, err
	}
	req, _, err := pipeline.BuildImageGenRequestRefs(t.ModelGroup, prompt, refs, gp.Resolution, gp.AspectRatio, labels, opt)
	return req, err
}

// kindNames 素材名。
var kindNames = map[model.Kind]string{model.KindTryon: "商拍换装", model.KindPose: "姿态复刻", model.KindImageGen: "图片生成"}

// saveAsset 成品写素材库（每张一行，source_task_id = 子任务号，entry_code 按 kind 取三个 studio 入口码）。
func (f *flow) saveAsset(ctx context.Context, t *model.Task, b *model.Batch, url string) {
	cfg := f.cfg.Asset
	if !cfg.Enabled || f.asset == nil {
		return
	}
	// 查重：交付在置成功前入库，置成功失败下一轮重来时这里已有一行，不再写第二条（查询失败照写，资产比去重要紧）
	if m, err := f.repo.Task.AssetIDs(ctx, b.UserID, []string{t.TaskID.String}); err != nil {
		logx.WithContext(ctx).Errorf("studio: 子任务 %d 入库前查重失败，照常写入: %v", t.ID, err)
	} else if m[t.TaskID.String] > 0 {
		return
	}
	p, _ := parsePayload(b.Payload)
	ar := ""
	if p != nil {
		ar = p.AspectRatio
	}
	props, _ := json.Marshal(map[string]any{
		"batchId": b.ID, "kind": t.Kind, "modelIndex": t.ModelIndex, "shotIndex": t.ShotIndex,
		"poseIndex": t.PoseIndex, "modelGroup": t.ModelGroup,
	})
	a := asset.Asset{
		UserID: b.UserID, SourceTaskID: t.TaskID.String, Type: orDefault(cfg.Type, "image"),
		EntryCode: svc.EntryCode(f.cfg, t.Kind), Name: kindNames[t.Kind], CoverURL: url, ResultURL: url,
		AspectRatio: ar, AIPrompt: PromptSummary(t), Properties: string(props),
	}
	if err := f.asset(ctx, []asset.Asset{a}); err != nil {
		logx.WithContext(ctx).Errorf("studio: 子任务 %d 写素材库失败: %v", t.ID, err)
	}
}

// ---------------------------------------------------------------- 提示词润色

// 润色模板沿用 Java /creation/optimize-prompt 的口径：fusion-aigc data/prompt/t2i.txt（无参考图）、i2i.txt（有参考图）原文。
// 待产品确认是否换成提示词平台（prompt_lab）里的已发布版本。
var (
	//go:embed prompts/t2i.txt
	rewriteT2I string
	//go:embed prompts/i2i.txt
	rewriteI2I string
)

// rewritePrompt 经 gateway chat.completion 润色一次（同步等待，≤ 反推时限）。返回润色后的提示词与成本（人民币）。
func (f *flow) rewritePrompt(ctx context.Context, batchID int64, prompt string, refs []pipeline.Image) (string, float64, error) {
	if !f.gwOn {
		return "", 0, errors.New("ai-gateway 未配置")
	}
	sys, group := rewriteT2I, f.cfg.Rewrite.ModelGroup
	user := []map[string]any{{"type": "text", "text": prompt}}
	if len(refs) > 0 { // 带图只能走 vlm.*（llm.* 传图会 route.no_capable_deployment）；图片块字段名是 uri
		sys, group = rewriteI2I, orDefault(f.cfg.Rewrite.VisionModelGroup, "vlm.qwen-3.8-flash")
		for _, im := range refs {
			part := map[string]any{"type": "image_url", "uri": "mr://" + im.RID}
			if im.SHA256 != "" {
				part["sha256"] = im.SHA256
			}
			user = append(user, part)
		}
	}
	req := aigateway.CallRequest{
		Operation: "chat.completion", ModelGroup: group,
		Input: map[string]any{"messages": []map[string]any{
			{"role": "system", "content": []map[string]any{{"type": "text", "text": sys}}},
			{"role": "user", "content": user},
		}},
		Labels: map[string]string{"batch": fmt.Sprint(batchID), "kind": "imagegen", "step": "rewrite"},
	}
	// 与生图同口径：deadline_at 兜底（不参与 request_hash），max_cost 封顶
	deadline := f.now().Add(f.analyzeTimeout() + 60*time.Second).UTC()
	req.Constraints = &aigateway.Constraints{DeadlineAt: &deadline}
	if mc := strings.TrimSpace(f.cfg.AiGateway.MaxCostCNY); mc != "" {
		req.Limits = &aigateway.Limits{MaxCost: mc}
	}
	cctx, cancel := context.WithTimeout(ctx, f.analyzeTimeout())
	defer cancel()
	call, err := f.gw.Submit(cctx, req, aigateway.SubmitOptions{IdempotencyKey: fmt.Sprintf("studio:%d:rewrite", batchID), Wait: f.longPollWait()})
	for err == nil && !call.IsTerminal() {
		call, err = f.gw.Get(cctx, call.ID, f.longPollWait())
	}
	if err != nil {
		return "", 0, err
	}
	cost := pipeline.CostCNY(call.Cost)
	if !call.Succeeded() || call.Result == nil {
		return "", cost, fmt.Errorf("润色调用 %s", call.Status)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := call.Result.Decode(&out); err != nil {
		return "", cost, err
	}
	text := parseRewrite(out.Message.Content)
	if text == "" {
		return "", cost, errors.New("润色结果为空")
	}
	return text, cost, nil
}

// parseRewrite 模板要求输出 {"prompt": "..."}（可能包在 ```json 里）；解析不了就用原文。
func parseRewrite(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var v struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal([]byte(s), &v) == nil && strings.TrimSpace(v.Prompt) != "" {
		return strings.TrimSpace(v.Prompt)
	}
	if strings.HasPrefix(s, "{") { // 像 JSON 但解析失败：不拿半截 JSON 当提示词
		return ""
	}
	return s
}
