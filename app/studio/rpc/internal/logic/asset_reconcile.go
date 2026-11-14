package logic

// 付费产物入库对账（pkg/asset.Reconciler）：成功且已确认扣费（billing_status=已结算）的子任务，素材库里没有 source_task_id = 子任务号
// 的行时补入。成品仍是私有引用 mr://（转存 outbox / 定时重投 / 用户一键重试负责）与免费档 mrw://（不扣费）不归这里。
// 补入前先抢子任务行租约（与交付同一把锁，交付续做期间不抢），抢到后重读子任务再走 saveAssetErr（同交付的查重口径）；
// 只看 GraceSeconds 之前结束的子任务，正常交付 / 转存都已入库完毕。

import (
	"context"
	"errors"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/asset"

	"github.com/zeromicro/go-zero/core/logx"
)

// reconcileLease 对账补入时持有子任务租约的时长（一次补入只是几条 SQL）。
const reconcileLease = time.Minute

// errReconcileBusy 子任务租约被别处持有（交付 / 运营重做中）：本次不补，下次扫描再看。
var errReconcileBusy = errors.New("子任务正被处理（租约被持有），下次再补")

// newAssetReconciler studio 对账器（素材库写入关闭时为 nil，Tick 空操作）。
func (f *flow) newAssetReconciler(w *asset.Writer) *asset.Reconciler {
	if !f.cfg.Asset.Enabled || w == nil {
		return nil
	}
	return &asset.Reconciler{
		Service: "studio", Conf: f.cfg.AssetReconcile, Writer: w, Now: f.now,
		List: func(ctx context.Context, from, to time.Time, after asset.ReconcileCursor, limit int) ([]asset.ReconcileItem, error) {
			cond, args := asset.KeysetCond("t.updated_at", "t.id", from, to, after)
			list, err := f.repo.Task.ListSettledSucceeded(ctx, cond, args, limit)
			if err != nil {
				return nil, err
			}
			out := make([]asset.ReconcileItem, 0, len(list))
			for _, t := range list {
				u := t.ResultURL.String
				out = append(out, asset.ReconcileItem{ID: t.ID, At: t.UpdatedAt, UserID: t.UserID, SourceTaskID: t.TaskID.String, Ref: t,
					Skip: u == "" || isPendingRef(u) || isWatermarkRef(u)})
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it asset.ReconcileItem) (bool, error) {
			return f.reconcileTask(ctx, it.ID)
		},
	}
}

// reconcileTask 抢租约 → 重读子任务（仍成功、已结算、成品是公开地址）→ 读批次 → saveAssetErr。
func (f *flow) reconcileTask(ctx context.Context, id int64) (bool, error) {
	ok, err := f.repo.Task.AcquireLease(ctx, id, f.owner, reconcileLease)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errReconcileBusy
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := f.repo.Task.ReleaseLease(rctx, id, f.owner); err != nil {
			logx.WithContext(ctx).Errorf("studio: 对账释放子任务 %d 租约失败（到期自动释放）: %v", id, err)
		}
	}()
	t, err := f.repo.Task.GetByID(ctx, id)
	if err != nil || t == nil {
		return false, err
	}
	u := t.ResultURL.String
	if t.Status != model.TaskSucceeded || t.BillingStatus != model.BillingSettled || u == "" || isPendingRef(u) || isWatermarkRef(u) {
		return false, nil
	}
	b, err := f.repo.Batch.GetByID(ctx, t.BatchID, 0)
	if err != nil {
		return false, err
	}
	if b == nil {
		b = &model.Batch{ID: t.BatchID, UserID: t.UserID}
	}
	if err := f.saveAssetErr(ctx, t, b, u); err != nil {
		return false, err
	}
	return true, nil
}
