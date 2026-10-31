// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRunsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 节点执行（run）列表
func NewListRunsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRunsLogic {
	return &ListRunsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListRunsLogic) ListRuns(req *types.CanvasAdminRunsReq) (resp *types.CanvasAdminRunsResp, err error) {
	out, err := l.svcCtx.CanvasRpc.AdminListRuns(l.ctx, &canvas_client.AdminListRunsReq{Page: req.Page, PageSize: req.PageSize, UserId: req.UserId,
		CanvasId: req.CanvasId, Kind: req.Kind, Status: req.Status, Backend: req.Backend, Start: req.Start, End: req.End})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasAdminRunsResp{Total: out.Total, List: make([]types.CanvasAdminRun, 0, len(out.List))}
	for _, a := range out.List {
		resp.List = append(resp.List, toRun(a))
	}
	return resp, nil
}
