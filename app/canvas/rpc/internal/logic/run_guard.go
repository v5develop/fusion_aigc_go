package logic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件是 Poller 的计费兜底（照 studio flow_guard.go）：在途 run 的预留单续期；终态 run 的计费镜像按预留单真实状态回写。

const (
	// renewEvery 续期间隔。Renew 把预留单顺延到「现在 + billing.LeaseDuration（15 分钟）」，1 分钟一次足够。
	renewEvery = time.Minute
	// renewConcurrency 续期并发（pkg/billing 没有批量接口，逐笔 UPDATE，限流）。
	renewConcurrency = 8
	// renewLimit 每轮最多续期多少笔。
	renewLimit = 5000
	// reconcileWindow 只核对最近这么久更新过的终态 run（预留单最长持有几十分钟、到期由 Reclaimer 释放，7 天绰绰有余；
	// 同时让查询走 idx_status(status, updated_at) 的范围扫描）。
	reconcileWindow = 7 * 24 * time.Hour

	// 预留单状态（pkg/billing 的常量未导出，这里用字面值；EXPIRED 是 Reclaimer 到期释放，CANCELLED 是取消释放）。
	resvHeld              = "HELD"
	resvConfirmed         = "CONFIRMED"
	resvPartiallyRefunded = "PARTIALLY_REFUNDED"
	resvRefunded          = "REFUNDED"
	resvExpired           = "EXPIRED"
	resvCancelled         = "CANCELLED"

	// unsettledCheck 「交付了没扣成费」告警的检索键。
	unsettledCheck = "canvas.unsettled_gone"
	// unsettledAlertCap 本进程记住多少个已告警的 run（超了清空重来，最坏多报一次）。
	unsettledAlertCap = 10000
)

// reconcileLimit 每轮最多核对多少笔「终态却仍是 review / held / unsettled」的 run（按 id 游标轮转；变量便于单测调小）。
var reconcileLimit = 500

// maintain 每 renewEvery 做一次：续期、计费镜像对账。
func (c *core) maintain(ctx context.Context) {
	now := c.now()
	if !c.renewAt.IsZero() && now.Sub(c.renewAt) < renewEvery {
		return
	}
	c.renewAt = now
	c.renewHeld(ctx)
	c.reconcileBilling(ctx)
}

// runBillingMirror 预留单状态 → run 计费镜像。ok=false 表示不动（仍 HELD、无单或未知状态）。
//
//	CONFIRMED / PARTIALLY_REFUNDED → settled；REFUNDED / EXPIRED / CANCELLED → refunded。
func runBillingMirror(resvStatus string) (to string, ok bool) {
	switch resvStatus {
	case resvConfirmed, resvPartiallyRefunded:
		return model.BillSettled, true
	case resvRefunded, resvExpired, resvCancelled:
		return model.BillRefunded, true
	}
	return "", false
}

// reconcileBilling 终态 run 的计费镜像对账：
//   - review / held：UNKNOWN 结果的 run 置失败后镜像是 review、预留单仍 HELD（不退款），到期由 Reclaimer 释放（EXPIRED）时
//     没人回写镜像，后台会一直显示「待对账」；运营人工确认扣费（CONFIRMED）同理。按预留单真实状态（billing.Status：只读 SQL，
//     biz_id 折叠规则同 pkg/billing）回写；仍 HELD 的不动（回收器还没到期）。
//   - unsettled：已交付但 Settle 遇暂时性错误，预留单可能仍 HELD——放着不管会被 Reclaimer 释放，等于交付了没扣费。
//     这里重试 Settle（HELD 确认扣费；已 CONFIRMED 幂等）：成功 → settled；ErrGone（已释放 / 已退）→ 保持 unsettled 并 ERROR 告警
//     供人工处理；其它错误下轮再试。
//
// 每轮按 id 游标取一页（取满记游标、下轮接着取；取不满回到开头），积压超过一页时不会被长期 HELD 的行饿死。
// 只用 CAS（billing_status=原值）写，不覆盖并发路径写入的结果。多实例重复做无害（Settle 幂等）。
func (c *core) reconcileBilling(ctx context.Context) {
	if c.s.Billing == nil || !c.s.Billing.On() {
		return
	}
	log := logx.WithContext(ctx)
	list, err := c.s.Repo.Run.ListFinalBillingOpen(ctx, c.now().Add(-reconcileWindow), c.reconcileAfter, reconcileLimit)
	if err != nil {
		log.Errorf("canvas: 列待对账计费镜像的 run 失败: %v", err)
		return
	}
	if len(list) < reconcileLimit {
		c.reconcileAfter = 0
	} else {
		c.reconcileAfter = list[len(list)-1].ID
	}
	for _, r := range list {
		if r.BillingStatus == model.BillUnsettled {
			c.resettle(ctx, r)
			continue
		}
		st, err := c.s.Billing.Status(ctx, r.UserID, r.BillingRequestID.String)
		if err != nil {
			log.Errorf("canvas: run %d 查预留单状态失败: %v", r.ID, err)
			continue
		}
		to, ok := runBillingMirror(st)
		if !ok {
			continue
		}
		c.casMirror(ctx, r, to, st)
	}
}

// casMirror CAS 回写计费镜像（billing_status 仍是 r.BillingStatus 才写）。
func (c *core) casMirror(ctx context.Context, r *model.NodeRun, to, resv string) {
	log := logx.WithContext(ctx)
	done, err := c.s.Repo.Run.CASBillingStatus(ctx, r.ID, r.BillingStatus, to)
	if err != nil {
		log.Errorf("canvas: run %d 回写计费镜像失败: %v", r.ID, err)
		return
	}
	if done {
		log.Infow("canvas billing mirror reconciled", logx.Field("run_id", r.ID),
			logx.Field("billing_request_id", r.BillingRequestID.String), logx.Field("reservation", resv),
			logx.Field("from", r.BillingStatus), logx.Field("billing_status", to))
	}
}

// resettle 已交付、确认扣费失败（unsettled）的 run 重试 Settle。
func (c *core) resettle(ctx context.Context, r *model.NodeRun) {
	log := logx.WithContext(ctx)
	bctx, cancel := detached(ctx)
	st, err := c.s.Billing.SettleCost(bctx, r.UserID, r.BillingRequestID.String, settleQuantity(r.Kind, len(r.CandidateList())), c.runCost(r, nil))
	cancel()
	switch {
	case err == nil:
		if st != nil && st.ActualCost && !st.Already {
			if uerr := c.s.Repo.Run.Update(ctx, r.ID, map[string]any{"points": st.Points}); uerr != nil {
				log.Errorf("canvas: run %d 写实扣点数 %d 失败: %v", r.ID, st.Points, uerr)
			}
		}
		c.casMirror(ctx, r, model.BillSettled, "settle_retry")
		delete(c.unsettledAlerted, r.ID)
	case errors.Is(err, billing.ErrGone):
		if c.unsettledAlerted == nil {
			c.unsettledAlerted = map[int64]bool{}
		}
		if c.unsettledAlerted[r.ID] {
			log.Infof("canvas: run %d 已交付但预留单 %s 已不可结算（已告警，待人工）: %v", r.ID, r.BillingRequestID.String, err)
			return
		}
		if len(c.unsettledAlerted) >= unsettledAlertCap {
			clear(c.unsettledAlerted)
		}
		c.unsettledAlerted[r.ID] = true
		log.Errorw(fmt.Sprintf("canvas: run %d 已交付但预留单已释放 / 已退，无法确认扣费，请人工处理 (check=%s)", r.ID, unsettledCheck),
			logx.Field("run_id", r.ID), logx.Field("user_id", r.UserID),
			logx.Field("billing_request_id", r.BillingRequestID.String), logx.Field("err", err.Error()))
	default:
		log.Errorf("canvas: run %d 重试确认扣费暂时失败（保持 unsettled，下轮再试）: %v", r.ID, err)
	}
}

// renewHeld 排队 / 运行中、已冻结（held）的 gateway run 逐笔续期预留单：排队久（如 gateway 繁忙一直暂时性失败）、
// 或运行时间接近 Hold 时长的 run 不会被回收器先收走，结算时也就不会 ErrGone。多实例重复续期无害。
func (c *core) renewHeld(ctx context.Context) {
	if c.s.Billing == nil || !c.s.Billing.On() {
		return
	}
	log := logx.WithContext(ctx)
	list, err := c.s.Repo.Run.ListHeldActive(ctx, renewLimit)
	if err != nil {
		log.Errorf("canvas: 列待续期 run 失败: %v", err)
		return
	}
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, renewConcurrency)
	)
	for _, r := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(r *model.NodeRun) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.s.Billing.Renew(ctx, r.UserID, r.BillingRequestID.String); err != nil {
				log.Errorf("canvas: run %d 续期失败: %v", r.ID, err)
			}
		}(r)
	}
	wg.Wait()
}
