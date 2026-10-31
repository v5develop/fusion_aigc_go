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

type CategoriesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分类条计数：all + 5 个 kind + uncategorized，随 source / media
func NewCategoriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CategoriesLogic {
	return &CategoriesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CategoriesLogic) Categories(req *types.AssetLibCategoriesReq) (resp *types.AssetLibCategoriesResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.Categories(l.ctx, &assetlib_client.CategoriesReq{UserId: ctxdata.UserID(l.ctx), Source: req.Source, Media: req.Media})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibCategoriesResp{Items: make([]types.AssetLibCategory, 0, len(out.Items))}
	for _, c := range out.Items {
		resp.Items = append(resp.Items, types.AssetLibCategory{Key: c.Key, Label: c.Label, Count: c.Count})
	}
	return resp, nil
}
