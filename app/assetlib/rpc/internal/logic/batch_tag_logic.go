package logic

import (
	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BatchTagLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchTagLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchTagLogic {
	return &BatchTagLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量给资产加标签（追加，不删已有）
func (l *BatchTagLogic) BatchTag(in *assetlib.BatchTagReq) (*assetlib.BatchTagResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	assetIDs, tagIDs := uniq(in.AssetIds), uniq(in.TagIds)
	if len(assetIDs) == 0 || len(tagIDs) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "asset_ids 与 tag_ids 都不能为空")
	}
	if len(assetIDs) > lim.BatchAssets {
		return nil, xerr.Newf(xerr.ParamError, "一次最多 %d 个资产", lim.BatchAssets)
	}
	if len(tagIDs) > lim.TagsPerAsset {
		return nil, xerr.Newf(xerr.ParamError, "一次最多 %d 个标签", lim.TagsPerAsset)
	}
	owned, err := l.svcCtx.Repo.Asset.OwnedIDs(l.ctx, in.UserId, assetIDs)
	if err != nil {
		return nil, err
	}
	if len(owned) != len(assetIDs) {
		return nil, errNotFound
	}
	tags, err := l.svcCtx.Repo.Tag.ByIDs(l.ctx, in.UserId, tagIDs)
	if err != nil {
		return nil, err
	}
	if len(tags) != len(tagIDs) {
		return nil, xerr.New(xerr.NotFound, "标签不存在")
	}
	var added int64
	err = l.svcCtx.Repo.RunInTx(l.ctx, func(ctx context.Context, tx *repo.Repo) error {
		cur, err := tx.Tag.TagsOf(ctx, in.UserId, assetIDs)
		if err != nil {
			return err
		}
		var links []*model.TagLink
		for _, aid := range assetIDs {
			have := map[int64]bool{}
			for _, t := range cur[aid] {
				have[t.TagID] = true
			}
			n := len(have)
			for _, tid := range tagIDs {
				if !have[tid] {
					n++
					links = append(links, &model.TagLink{AssetID: aid, TagID: tid})
				}
			}
			if n > lim.TagsPerAsset {
				return xerr.Newf(xerr.ParamError, "资产 %d 的标签会超过 %d 个", aid, lim.TagsPerAsset)
			}
		}
		added, err = tx.Tag.AddLinks(ctx, in.UserId, links)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &assetlib.BatchTagResp{Added: added}, nil
}
