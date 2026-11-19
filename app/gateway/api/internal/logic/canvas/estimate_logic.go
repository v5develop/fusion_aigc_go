package canvas

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type EstimateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生图预估点数（只读，不冻结）：与执行时冻结同一个 Quote
func NewEstimateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EstimateLogic {
	return &EstimateLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

func (l *EstimateLogic) Estimate(req *types.CanvasEstimateReq) (resp *types.CanvasEstimateResp, err error) {
	var sizes []*canvas_client.ImageSize
	for _, z := range req.RefImageSizes {
		sizes = append(sizes, &canvas_client.ImageSize{W: z.W, H: z.H})
	}
	out, err := l.svcCtx.CanvasRpc.Estimate(rpcCtx(l.ctx), &canvas_client.EstimateReq{RefImageSizes: sizes,
		UserId: ctxdata.UserID(l.ctx), Model: req.Model, ModelId: req.ModelId, ModelGroup: req.ModelGroup,
		Size: req.Size, Count: req.Count, Quality: req.Quality, RefImages: req.RefImages, PromptChars: req.PromptChars,
	})
	if err != nil {
		return nil, err
	}
	return &types.CanvasEstimateResp{
		Available: out.Available, Reason: out.Reason, Points: out.Points, CapPoints: out.CapPoints,
		SettleMode: out.SettleMode, Currency: out.Currency, BillingMode: out.BillingMode, Note: out.Note,
		Breakdown: types.CanvasEstimateBreakdown{ModelGroup: out.ModelGroup, Size: out.Size, Quality: out.Quality,
			Count: out.Count, RefImages: out.RefImages, UnitPoints: out.UnitPoints},
	}, nil
}
