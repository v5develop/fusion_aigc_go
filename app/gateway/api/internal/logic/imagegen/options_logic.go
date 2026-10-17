// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 海报/长图表单配置
func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OptionsLogic) Options(req *types.ImageGenOptionsReq) (resp *types.ImageGenOptionsResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.Options(l.ctx, &imagegen.KindReq{Kind: req.Kind})
	if err != nil {
		return nil, err
	}
	return &types.ImageGenOptionsResp{
		PosterTypes: toOptions(out.PosterTypes), PosterStyles: toOptions(out.PosterStyles), Sizes: toOptions(out.Sizes),
		DefaultSize: out.DefaultSize, MaxImages: out.MaxImages, MaxMaterials: out.MaxMaterials,
		MaxContentLen: out.MaxContentLen, TemplateRequired: out.TemplateRequired, Models: toOptions(out.Models),
		DefaultModel: out.DefaultModel, Durations: orEmptyInt(out.Durations), DefaultDuration: out.DefaultDuration,
	}, nil
}
