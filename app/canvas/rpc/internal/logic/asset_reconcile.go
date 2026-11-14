package logic

// 付费产物入库对账（pkg/asset.Reconciler）：成功且已确认扣费（billing_status=settled）的 Go 生成 run（图 / 视频 / 裁剪），
// 素材库里没有 source_task_id = task_id 的行时幂等补入（insertAssetsOnce：同 task_id 只写一组、锁定读查重，与转存 outbox 并发也只写一组）。
// 产物仍是私有引用 mr:// 的 run 不归这里：素材库不写私有引用，它们由转存 outbox / 定时重投 / 用户一键重试负责。
// 没有 task_id 的 run 由对账器打 ERROR check=ASSET_RECONCILE_NO_SOURCE（见 assetList 的 C3 说明）。

import (
	"context"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/asset"
)

// reconcileKinds 入素材库的 run 类别（与 assetList 一致）。
var reconcileKinds = []model.RunKind{model.KindImage, model.KindVideo, model.KindToolCrop}

// newAssetReconciler 画布对账器（素材库写入关闭时为 nil，Tick 空操作）。
func (c *core) newAssetReconciler() *asset.Reconciler {
	if !c.s.Config.Asset.Enabled || c.s.Asset == nil {
		return nil
	}
	return &asset.Reconciler{
		Service: "canvas", Conf: c.s.Config.AssetReconcile, Writer: c.s.Asset, Now: c.now,
		List: func(ctx context.Context, from, to time.Time, after asset.ReconcileCursor, limit int) ([]asset.ReconcileItem, error) {
			cond, args := asset.KeysetCond("r.updated_at", "r.id", from, to, after)
			runs, err := c.s.Repo.Run.ListSettledFinished(ctx, reconcileKinds, cond, args, limit)
			if err != nil {
				return nil, err
			}
			out := make([]asset.ReconcileItem, 0, len(runs))
			for _, r := range runs {
				it := asset.ReconcileItem{ID: r.ID, At: r.UpdatedAt, UserID: r.UserID, SourceTaskID: r.TaskID.String, Ref: r}
				it.Skip = hasPendingRefs(r.CandidateList(), r.VideoURL.String) // 仍是私有引用：转存 outbox 负责
				out = append(out, it)
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it asset.ReconcileItem) (bool, error) {
			r, ok := it.Ref.(*model.NodeRun)
			if !ok || r == nil {
				return false, nil
			}
			return c.insertAssetsOnce(ctx, r, r.CandidateList(), r.VideoURL.String)
		},
	}
}
