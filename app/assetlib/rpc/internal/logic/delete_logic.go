package logic

import (
	"context"

	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteLogic {
	return &DeleteLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// errOfficialUndeletable 官方素材不允许用户删除（单删口径）。
var errOfficialUndeletable = xerr.New(xerr.Forbidden, "官方素材不可删除")

// Delete 删除资产：逻辑删除 afv_asset（deleted = 1）并解除本人标签关联，同一事务。
// 只删本人的上传 / 生成资产（source_type 1 / 2）；官方素材（source_type 为空或其它）一律不删。
// 不删存储对象、不碰任务结果 / 免费档原图与水印副本（它们挂在任务行与存储上，不依赖资产行）、
// 不删版本 / 关系 / ext 旁表（读侧都只认未删除资产，留着便于追溯）。
func (l *DeleteLogic) Delete(in *assetlib.DeleteReq) (*assetlib.DeleteResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	ids := uniq(in.AssetIds)
	if len(ids) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "asset_ids 不能为空")
	}
	if lim := l.svcCtx.Config.Limits.BatchAssets; len(ids) > lim {
		return nil, xerr.Newf(xerr.ParamError, "一次最多删除 %d 个资产", lim)
	}
	if in.Strict && len(ids) != 1 {
		return nil, xerr.Param("单个删除只能带一个资产")
	}
	rows, err := l.svcCtx.Repo.Asset.OwnedSources(l.ctx, in.UserId, ids)
	if err != nil {
		return nil, err
	}
	deletable, official := splitDeletable(rows)
	missing := int64(len(ids) - len(rows))
	if in.Strict {
		if missing > 0 {
			l.Infof("[assetlib] 删除拒收：资产不存在或不属于本人（判据 user_id + deleted=0 + 非草稿） user=%d id=%d", in.UserId, ids[0])
			return nil, errNotFound
		}
		if official > 0 {
			l.Infof("[assetlib] 删除拒收：官方素材（判据 source_type 不是 1/2） user=%d id=%d", in.UserId, ids[0])
			return nil, errOfficialUndeletable
		}
	}
	var deleted int64
	if len(deletable) > 0 {
		err = l.svcCtx.Repo.RunInTx(l.ctx, func(ctx context.Context, tx *repo.Repo) error {
			n, err := tx.Asset.SoftDelete(ctx, in.UserId, deletable)
			if err != nil {
				return err
			}
			deleted = n
			_, err = tx.Tag.UnlinkAssets(ctx, in.UserId, deletable)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	l.Infof("[assetlib] 删除资产 user=%d 请求=%d 删除=%d 官方跳过=%d 不存在=%d", in.UserId, len(ids), deleted, official, missing)
	if deletable == nil {
		deletable = []int64{}
	}
	return &assetlib.DeleteResp{Deleted: deleted, OfficialSkipped: official, Missing: missing, DeletedIds: deletable}, nil
}

// splitDeletable 本人资产按来源分流：上传 / 生成可删，其余（官方）计数跳过。
func splitDeletable(rows []repo.OwnedSource) (deletable []int64, official int64) {
	for _, r := range rows {
		if r.SourceType.Valid && (r.SourceType.Int64 == model.SourceTypeUpload || r.SourceType.Int64 == model.SourceTypeGenerated) {
			deletable = append(deletable, r.ID)
		} else {
			official++
		}
	}
	return deletable, official
}
