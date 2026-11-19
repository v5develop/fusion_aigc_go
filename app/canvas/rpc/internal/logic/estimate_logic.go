package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type EstimateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewEstimateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EstimateLogic {
	return &EstimateLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 生图预估点数（只读，与冻结同一个 Quote；契约 §AE）。
func (l *EstimateLogic) Estimate(in *canvas.EstimateReq) (*canvas.EstimateResp, error) {
	return newCore(l.svcCtx).Estimate(l.ctx, in)
}
