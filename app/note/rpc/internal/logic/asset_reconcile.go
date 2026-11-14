package logic

// 付费产物入库对账（pkg/asset.Reconciler）：已完成的笔记（afv_image_text_note.status=4）。「已完成」只在确认扣费成功之后才写
// （pollGenerates：先 Settle 再置完成，Settle 失败置生成失败），所以 = 成功且已确认扣费（计费关闭时没有扣费，照样入库，与收尾一致）。
// 用户删除的笔记（deleted=1，素材随之软删）不补。补入走 InsertSetOnceIdx（source_task_id + 产物序号幂等、锁定读），
// 与收尾的同事务入库并发也只写一组。

import (
	"context"
	"time"

	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/pkg/asset"
)

// newAssetReconciler 笔记对账器（素材库写入关闭时为 nil，Tick 空操作）。
func newAssetReconciler(svcCtx *svc.ServiceContext) *asset.Reconciler {
	if !svcCtx.Config.Asset.Enabled || svcCtx.Asset == nil {
		return nil
	}
	return &asset.Reconciler{
		Service: "note", Conf: svcCtx.Config.AssetReconcile, Writer: svcCtx.Asset,
		List: func(ctx context.Context, from, to time.Time, after asset.ReconcileCursor, limit int) ([]asset.ReconcileItem, error) {
			cond, args := asset.KeysetCond("`update_time`", "`id`", from, to, after)
			rows, err := svcCtx.Note.ListFinished(ctx, model.StatusDone, cond, args, limit)
			if err != nil {
				return nil, err
			}
			out := make([]asset.ReconcileItem, 0, len(rows))
			for _, n := range rows {
				out = append(out, asset.ReconcileItem{ID: n.Id, At: n.UpdateTime, UserID: n.UserId, SourceTaskID: n.GenTaskId.String, Ref: n})
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it asset.ReconcileItem) (bool, error) {
			n := it.Ref.(*model.AfvImageTextNote)
			list := noteAssets(svcCtx, n, parseList(n.ImageUrls))
			if len(list) == 0 {
				return false, nil
			}
			c, err := svcCtx.Asset.InsertSetOnceIdx(ctx, list)
			return c > 0, err
		},
	}
}
