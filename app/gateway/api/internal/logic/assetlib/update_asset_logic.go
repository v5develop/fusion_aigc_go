// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateAssetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 改标题 / 分类 / 收藏 / 标签 / 元数据 / 授权 / 当前版本（只改带了的字段）
func NewUpdateAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateAssetLogic {
	return &UpdateAssetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateAssetLogic) UpdateAsset(req *types.AssetLibUpdateReq) (resp *types.AssetLibAsset, err error) {
	in := &assetlib_client.UpdateReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx), Title: req.Title, Kind: req.Kind, Favorite: req.Favorite,
		CurrentVersionId: req.CurrentVersionId, License: req.License}
	if req.Tags != nil {
		in.Tags = &assetlib_client.TagNames{Names: *req.Tags}
	}
	if m := req.Metadata; m != nil {
		in.Metadata = &assetlib_client.Metadata{Industry: m.Industry, Style: m.Style, Shot: m.Shot, Color: m.Color}
	}
	out, err := l.svcCtx.AssetLibRpc.Update(l.ctx, in)
	if err != nil {
		return nil, err
	}
	a := toAsset(out)
	return &a, nil
}
