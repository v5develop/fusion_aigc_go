package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminCostSummaryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminCostSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminCostSummaryLogic {
	return &AdminCostSummaryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminCostSummaryLogic) AdminCostSummary(in *canvas.AdminCostSummaryReq) (*canvas.AdminCostSummaryResp, error) {
	return newCore(l.svcCtx).AdminCostSummary(l.ctx, in)
}
