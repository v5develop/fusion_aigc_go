package logic

import (
	"context"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- App ----
func (l *OptionsLogic) Options(in *pb.KindReq) (*pb.OptionsResp, error) {
	cfg, err := kindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	resp := &pb.OptionsResp{
		DefaultSize: defaultSizeOf(l.ctx, l.svcCtx, in.Kind, cfg.DefaultSize), MaxImages: int32(lim.MaxImages), MaxMaterials: int32(lim.MaxMaterials),
		MaxContentLen: int32(lim.MaxContentLen), TemplateRequired: templateRequired(l.ctx, l.svcCtx, in.Kind, cfg.TemplateRequired),
	}
	if in.Kind == model.KindActionTransfer { // 动作迁移：只有模型选择
		resp.Models = optionsOf(l.ctx, l.svcCtx, genActionModels, actionModels)
		resp.MaxImages = 1
		return resp, nil
	}
	if in.Kind == model.KindVoiceover { // 数字人口播：模型、时长、比例
		vm := optionsOf(l.ctx, l.svcCtx, genVoiceoverModels, voiceoverModels)
		resp.Models, resp.DefaultModel = vm, defaultOf(l.ctx, l.svcCtx, genVoiceoverModels, vm[0].Code)
		resp.Durations = durationsOf(l.ctx, l.svcCtx, genVoiceoverDurations, voiceoverDurations)
		resp.DefaultDuration = defaultDurationOf(l.ctx, l.svcCtx, genVoiceoverDurations, voiceoverDurations[0])
		resp.Sizes = optionsOf(l.ctx, l.svcCtx, genVoiceoverSizes, voiceoverSizes)
		resp.MaxImages, resp.MaxMaterials = 1, 0
		resp.MaxContentLen = 0 // 口播文案字数不限
		return resp, nil
	}
	if in.Kind == model.KindLongImage { // 长图比例固定，设计稿没有比例与风格选项
		resp.Sizes = optionsOf(l.ctx, l.svcCtx, genLongImageSizes, []*pb.Option{{Code: cfg.DefaultSize, Label: cfg.DefaultSize}})
		// 长图类型（喜庆 / 营销 / 社媒）来自长图筛选维度，沿用 poster_types 字段下发，与模板列表的 poster_type 参数配对
		types, err := l.svcCtx.Shared.ListFilterOptions(l.ctx, cfg.BizType, longImageTypeFilter)
		if err != nil {
			return nil, err
		}
		for _, o := range types {
			resp.PosterTypes = append(resp.PosterTypes, &pb.Option{Code: o.Code, Label: o.Name})
		}
		return resp, nil
	}
	// 海报：档位由后端下发（前端不写死）；长图没有 model 参数，上面已 return
	resp.Models = optionsOf(l.ctx, l.svcCtx, genPosterModels, posterModels)
	resp.DefaultModel = defaultOf(l.ctx, l.svcCtx, genPosterModels, cfg.Model)
	resp.Sizes, resp.PosterStyles = optionsOf(l.ctx, l.svcCtx, genPosterSizes, posterSizes), posterStyles
	// 海报类型来自模板筛选维度字典，运营在现有后台维护
	opts, err := l.svcCtx.Shared.ListFilterOptions(l.ctx, cfg.BizType, posterTypeFilter)
	if err != nil {
		return nil, err
	}
	for _, o := range opts {
		resp.PosterTypes = append(resp.PosterTypes, &pb.Option{Code: o.Code, Label: o.Name})
	}
	return resp, nil
}
