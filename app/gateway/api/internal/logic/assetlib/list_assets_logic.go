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

type ListAssetsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 资产列表（kind / source / media / tag / 收藏 / 关键字，page 分页）
func NewListAssetsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAssetsLogic {
	return &ListAssetsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListAssetsLogic) ListAssets(req *types.AssetLibListReq) (resp *types.AssetLibListResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.List(l.ctx, &assetlib_client.ListReq{UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, Source: req.Source,
		Media: req.Media, Tag: req.Tag, FavoriteOnly: req.FavoriteOnly, Q: req.Q, Page: req.Page, PageSize: req.PageSize,
		KindGroup: req.KindGroup, AspectRatio: req.AspectRatio})
	if err != nil {
		return nil, err
	}
	return &types.AssetLibListResp{Items: toAssets(out.Items), Total: out.Total}, nil
}
