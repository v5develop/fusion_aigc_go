package logic

import (
	"context"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"

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
		DefaultSize: cfg.DefaultSize, MaxImages: int32(lim.MaxImages), MaxMaterials: int32(lim.MaxMaterials),
		MaxContentLen: int32(lim.MaxContentLen), TemplateRequired: cfg.TemplateRequired,
	}
	if in.Kind == model.KindActionTransfer { // 动作迁移：只有模型选择
		resp.Models = actionModels
		resp.MaxImages = 1
		return resp, nil
	}
	if in.Kind == model.KindVoiceover { // 数字人口播：模型、时长、比例
		resp.Models, resp.DefaultModel = voiceoverModels, voiceoverModels[0].Code
		resp.Durations, resp.DefaultDuration = voiceoverDurations, voiceoverDurations[0]
		resp.Sizes = voiceoverSizes
		resp.MaxImages, resp.MaxMaterials = 1, 0
		resp.MaxContentLen = 0 // 口播文案字数不限
		return resp, nil
	}
	if in.Kind == model.KindLongImage { // 长图比例固定，设计稿没有比例与风格选项
		resp.Sizes = []*pb.Option{{Code: cfg.DefaultSize, Label: cfg.DefaultSize}}
		return resp, nil
	}
	// 海报：档位由后端下发（前端不写死）；长图没有 model 参数，上面已 return
	resp.Models, resp.DefaultModel = posterModels, cfg.Model
	resp.Sizes, resp.PosterStyles = posterSizes, posterStyles
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
