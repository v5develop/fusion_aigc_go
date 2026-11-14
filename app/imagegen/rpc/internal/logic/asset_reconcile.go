package logic

// 付费产物入库对账（pkg/asset.Reconciler）：海报 / 长图 / 动作迁移 / 数字人口播（afv_local_life_creation）与
// 探店 / 短剧 / 爆款复刻出片（afv_video_creation）。两张表的「已完成」都只在确认扣费成功之后才写（pollOne / pollVideoGenerates：
// 先 Settle 再置完成，Settle 失败置失败），所以「已完成」= 成功且已确认扣费（计费关闭时没有扣费，照样入库，与收尾一致）。
// 用户删除的记录（deleted=1，素材随之软删）不补。补入走 InsertSetOnceIdx（source_task_id + 产物序号幂等、锁定读），
// 与收尾的同事务入库并发也只写一组。

import (
	"context"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/pkg/asset"
)

// newAssetReconcilers 两张表各一个对账器（素材库写入关闭时为空）。
func newAssetReconcilers(svcCtx *svc.ServiceContext) []*asset.Reconciler {
	if !svcCtx.Config.Asset.Enabled || svcCtx.Asset == nil {
		return nil
	}
	conf := svcCtx.Config.AssetReconcile
	images := &asset.Reconciler{
		Service: "imagegen", Conf: conf, Writer: svcCtx.Asset,
		List: func(ctx context.Context, from, to time.Time, after asset.ReconcileCursor, limit int) ([]asset.ReconcileItem, error) {
			cond, args := asset.KeysetCond("`update_time`", "`id`", from, to, after)
			rows, err := svcCtx.Creation.ListFinished(ctx, model.StatusDone, cond, args, limit)
			if err != nil {
				return nil, err
			}
			out := make([]asset.ReconcileItem, 0, len(rows))
			for _, c := range rows {
				out = append(out, asset.ReconcileItem{ID: c.Id, At: c.UpdateTime, UserID: c.UserId, SourceTaskID: groupTaskID(c), Ref: c})
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it asset.ReconcileItem) (bool, error) {
			c := it.Ref.(*model.LocalLifeCreation)
			list := assetsOf(svcCtx, c, it.SourceTaskID, parseList(c.ImageUrls))
			if len(list) == 0 {
				return false, nil
			}
			n, err := svcCtx.Asset.InsertSetOnceIdx(ctx, list)
			return n > 0, err
		},
	}
	videos := &asset.Reconciler{
		Service: "imagegen_video", Conf: conf, Writer: svcCtx.Asset,
		List: func(ctx context.Context, from, to time.Time, after asset.ReconcileCursor, limit int) ([]asset.ReconcileItem, error) {
			cond, args := asset.KeysetCond("`update_time`", "`id`", from, to, after)
			rows, err := svcCtx.Video.ListFinished(ctx, model.VideoDone, cond, args, limit)
			if err != nil {
				return nil, err
			}
			out := make([]asset.ReconcileItem, 0, len(rows))
			for _, c := range rows {
				out = append(out, asset.ReconcileItem{ID: c.Id, At: c.UpdateTime, UserID: c.UserId, SourceTaskID: c.GenTaskId.String, Ref: c})
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it asset.ReconcileItem) (bool, error) {
			c := it.Ref.(*model.AfvVideoCreation)
			// 置完成时抽出的代表帧已写进 cover_url（FinishGenerate COALESCE），这里 frame 传空即得到同一封面
			list := videoAssetsOf(svcCtx, c, c.VideoUrl.String, "")
			if len(list) == 0 {
				return false, nil
			}
			n, err := svcCtx.Asset.InsertSetOnceIdx(ctx, list)
			return n > 0, err
		},
	}
	return []*asset.Reconciler{images, videos}
}
