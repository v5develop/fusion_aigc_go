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

type ListRunsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 画布的执行记录（新的在前）
func NewListRunsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRunsLogic {
	return &ListRunsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListRunsLogic) ListRuns(req *types.CanvasRunsReq) (resp *types.CanvasRunsResp, err error) {
	out, err := l.svcCtx.CanvasRpc.ListRuns(rpcCtx(l.ctx), &canvas_client.ListRunsReq{UserId: ctxdata.UserID(l.ctx), CanvasId: req.Id, NodeId: req.NodeId, Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasRunsResp{Items: make([]types.CanvasRunResult, 0, len(out.List))}
	for _, r := range out.List {
		resp.Items = append(resp.Items, toRunResult(r))
	}
	return resp, nil
}
