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

type ListCanvasesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 画布列表（updated_at 倒序）
func NewListCanvasesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCanvasesLogic {
	return &ListCanvasesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListCanvasesLogic) ListCanvases(req *types.CanvasListReq) (resp *types.CanvasListResp, err error) {
	out, err := l.svcCtx.CanvasRpc.ListCanvases(rpcCtx(l.ctx), &canvas_client.ListCanvasesReq{UserId: ctxdata.UserID(l.ctx), Page: req.Page, PageSize: req.PageSize})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasListResp{Total: out.Total, Items: make([]types.CanvasSummary, 0, len(out.List))}
	for _, c := range out.List {
		resp.Items = append(resp.Items, types.CanvasSummary{Id: c.Id, Title: c.Title, Revision: c.Revision, NodeCount: c.NodeCount,
			CoverUrl: c.CoverUrl, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return resp, nil
}
