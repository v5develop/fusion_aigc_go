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

type CreateFromUrlLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 把自己的一张图存为资产（画布「存为资产」）：归属 / 落桶 / 去重全在 assetlib-rpc 里做
func NewCreateFromUrlLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateFromUrlLogic {
	return &CreateFromUrlLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateFromUrlLogic) CreateFromUrl(req *types.AssetLibFromUrlReq) (resp *types.AssetLibFromUrlResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.CreateFromUrl(l.ctx, &assetlib_client.CreateFromUrlReq{UserId: ctxdata.UserID(l.ctx),
		Url: req.Url, Title: req.Title, Source: req.Source, CanvasId: req.CanvasId, NodeId: req.NodeId, Tags: req.Tags})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibFromUrlResp{Existing: out.Existing}
	if out.Asset != nil {
		resp.Asset = toAsset(out.Asset)
	}
	return resp, nil
}
