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

type CostSummaryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 成本汇总（按 kind 或 model_group）
func NewCostSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CostSummaryLogic {
	return &CostSummaryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CostSummaryLogic) CostSummary(req *types.CanvasAdminCostReq) (resp *types.CanvasAdminCostResp, err error) {
	out, err := l.svcCtx.CanvasRpc.AdminCostSummary(l.ctx, &canvas_client.AdminCostSummaryReq{Start: req.Start, End: req.End, GroupBy: req.GroupBy})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasAdminCostResp{Rows: make([]types.CanvasAdminCostRow, 0, len(out.Rows))}
	for _, r := range out.Rows {
		resp.Rows = append(resp.Rows, types.CanvasAdminCostRow{Key: r.Key, Runs: r.Runs, Succeeded: r.Succeeded, Failed: r.Failed,
			Canceled: r.Canceled, CostCny: r.CostCny, Points: r.Points})
	}
	return resp, nil
}
