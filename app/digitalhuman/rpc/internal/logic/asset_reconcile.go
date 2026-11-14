package logic

// 付费产物入库对账（pkg/asset.Reconciler）：已完成的数字人口播视频（afv_digital_human.status=DhStatusDone）。「已完成」只在确认扣费
// 成功之后才写（pollVideos：先 Settle 再置完成，Settle 失败置失败），所以 = 成功且已确认扣费（计费关闭时没有扣费，照样入库，与收尾一致）。
// AI 定制形象不扣费，不在对账之列。用户删除的（deleted=1，素材随之软删）不补。补入走 InsertSetOnceIdx（source_task_id + 产物序号幂等、
// 锁定读），与收尾的同事务入库并发也只写一组。

import (
	"context"
	"time"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/pkg/asset"
)

// newAssetReconciler 口播视频对账器（素材库写入关闭时为 nil，Tick 空操作）。
func newAssetReconciler(svcCtx *svc.ServiceContext) *asset.Reconciler {
	if !svcCtx.Config.Asset.Enabled || svcCtx.Asset == nil {
		return nil
	}
	return &asset.Reconciler{
		Service: "digitalhuman", Conf: svcCtx.Config.AssetReconcile, Writer: svcCtx.Asset,
		List: func(ctx context.Context, from, to time.Time, after asset.ReconcileCursor, limit int) ([]asset.ReconcileItem, error) {
			cond, args := asset.KeysetCond("`update_time`", "`id`", from, to, after)
			rows, err := svcCtx.Human.ListFinished(ctx, model.DhStatusDone, cond, args, limit)
			if err != nil {
				return nil, err
			}
			out := make([]asset.ReconcileItem, 0, len(rows))
			for _, h := range rows {
				out = append(out, asset.ReconcileItem{ID: h.Id, At: h.UpdateTime, UserID: h.UserId, SourceTaskID: h.VideoTaskId.String, Ref: h})
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it asset.ReconcileItem) (bool, error) {
			h := it.Ref.(*model.AfvDigitalHuman)
			list := videoAssets(svcCtx, h, h.VideoUrl.String)
			if len(list) == 0 {
				return false, nil
			}
			n, err := svcCtx.Asset.InsertSetOnceIdx(ctx, list)
			return n > 0, err
		},
	}
}
