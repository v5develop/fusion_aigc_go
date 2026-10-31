package logic

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"

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
	// reconcileLimit 每轮最多核对多少笔「终态却仍是 review / held」的 run。
	reconcileLimit = 500
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
)

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

// reconcileBilling 终态 run 的计费镜像对账：UNKNOWN 结果的 run 置失败后镜像是 review、预留单仍 HELD（不退款），
// 到期由 Reclaimer 释放（EXPIRED）时没人回写镜像，后台会一直显示「待对账」；运营人工确认扣费（CONFIRMED）同理。
// 这里挑出「已终态且镜像 ∈ {review, held}」的 run，按预留单真实状态（billing.Status：只读 SQL，biz_id 折叠规则同 pkg/billing）
// 回写；仍 HELD 的不动（回收器还没到期）。只用 CAS（billing_status=原值）写，不覆盖并发路径写入的结果。多实例重复做无害。
func (c *core) reconcileBilling(ctx context.Context) {
	if c.s.Billing == nil || !c.s.Billing.On() {
		return
	}
	log := logx.WithContext(ctx)
	list, err := c.s.Repo.Run.ListFinalBillingOpen(ctx, c.now().Add(-reconcileWindow), reconcileLimit)
	if err != nil {
		log.Errorf("canvas: 列待对账计费镜像的 run 失败: %v", err)
		return
	}
	for _, r := range list {
		st, err := c.s.Billing.Status(ctx, r.UserID, r.BillingRequestID.String)
		if err != nil {
			log.Errorf("canvas: run %d 查预留单状态失败: %v", r.ID, err)
			continue
		}
		to, ok := runBillingMirror(st)
		if !ok {
			continue
		}
		done, err := c.s.Repo.Run.CASBillingStatus(ctx, r.ID, r.BillingStatus, to)
		if err != nil {
			log.Errorf("canvas: run %d 回写计费镜像失败: %v", r.ID, err)
			continue
		}
		if done {
			log.Infow("canvas billing mirror reconciled", logx.Field("run_id", r.ID),
				logx.Field("billing_request_id", r.BillingRequestID.String), logx.Field("reservation", st),
				logx.Field("from", r.BillingStatus), logx.Field("billing_status", to))
		}
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
