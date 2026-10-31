// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"

	"github.com/zeromicro/go-zero/core/logx"
)

type CostSummaryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 成本汇总（按类型或 model group）
func NewCostSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CostSummaryLogic {
	return &CostSummaryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CostSummaryLogic) CostSummary(req *types.StudioCostSummaryReq) (resp *types.StudioCostSummaryResp, err error) {
	out, err := l.svcCtx.StudioRpc.AdminCostSummary(l.ctx, &studio_client.AdminCostSummaryReq{
		DateRange: &studio_client.DateRange{Start: req.StartAt, End: req.EndAt}, GroupBy: req.GroupBy,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.StudioCostSummaryResp{Rows: make([]types.StudioCostRow, 0, len(out.Rows)), Total: toCostRow(out.Total)}
	for _, r := range out.Rows {
		resp.Rows = append(resp.Rows, toCostRow(r))
	}
	return resp, nil
}
