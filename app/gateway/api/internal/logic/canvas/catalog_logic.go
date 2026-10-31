// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type CatalogLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模型 / 参数 / 工具目录（替代前端写死）
func NewCatalogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CatalogLogic {
	return &CatalogLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CatalogLogic) Catalog() (resp *types.CanvasCatalogResp, err error) {
	out, err := l.svcCtx.CanvasRpc.Catalog(rpcCtx(l.ctx), &canvas_client.CatalogReq{UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasCatalogResp{ImageModels: []types.CanvasCatalogModel{}, VideoModels: []types.CanvasCatalogModel{}, Tools: []types.CanvasCatalogTool{},
		JavaImageConfigs: rawJSON(out.JavaImageConfigs), JavaVideoConfigs: rawJSON(out.JavaVideoConfigs)}
	for _, m := range out.ImageModels {
		resp.ImageModels = append(resp.ImageModels, toCatalogModel(m))
	}
	for _, m := range out.VideoModels {
		resp.VideoModels = append(resp.VideoModels, toCatalogModel(m))
	}
	for _, t := range out.Tools {
		resp.Tools = append(resp.Tools, types.CanvasCatalogTool{Tool: t.Tool, Label: t.Label, Billing: t.Billing, Scales: t.Scales, Ratios: t.Ratios, Ready: t.Ready})
	}
	return resp, nil
}
