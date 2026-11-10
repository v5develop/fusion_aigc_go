// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ModelsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模型档能力（只读）：每个模型档的展示名、实际可用的分辨率 / 比例、参考图上限；with_price=true 附单张点数
func NewModelsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ModelsLogic {
	return &ModelsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ModelsLogic) Models(req *types.StudioModelsReq) (resp *types.StudioModelsResp, err error) {
	out, err := l.svcCtx.StudioRpc.Models(l.ctx, &studio_client.ModelsReq{UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, WithPrice: req.WithPrice})
	if err != nil {
		return nil, err
	}
	resp = &types.StudioModelsResp{Kind: out.Kind, BillingMode: out.BillingMode, Items: make([]types.StudioModelCapability, 0, len(out.Items))}
	for _, it := range out.Items {
		c := types.StudioModelCapability{
			Code: it.Code, Label: it.Label, Available: it.Available, // model_group 是内部模型组码，不下发给用户
			Resolutions: nonNilStrs(it.Resolutions), AspectRatios: nonNilStrs(it.AspectRatios), AutoAspect: it.AutoAspect,
			MaxRefImages: it.MaxRefImages, Notes: nonNilStrs(it.Notes), Prices: make([]types.StudioModelResolutionPrice, 0, len(it.Prices)),
		}
		for _, p := range it.Prices {
			c.Prices = append(c.Prices, types.StudioModelResolutionPrice{Resolution: p.Resolution, Available: p.Available, UnitPoints: p.UnitPoints, SettleMode: p.SettleMode})
		}
		resp.Items = append(resp.Items, c)
	}
	return resp, nil
}

func nonNilStrs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
