package logic

import (
	"context"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type VideoOptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVideoOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VideoOptionsLogic {
	return &VideoOptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 探店视频（72→73）/ 带货短剧（87→88）：两步，先出剧本再出片 ----
func (l *VideoOptionsLogic) VideoOptions(in *pb.KindReq) (*pb.VideoOptionsResp, error) {
	if in.Kind == model.KindVideoRemake {
		c := &l.svcCtx.Config.VideoRemake
		lim := videoLimits(c)
		rm := optionsOf(l.ctx, l.svcCtx, genRemakeModels, remakeModels)
		return &pb.VideoOptionsResp{
			MaxVideoBytes: lim.MaxBytes, MaxVideoSeconds: int32(lim.MaxSeconds),
			VideoFormats: lim.Formats, LinkEnabled: l.svcCtx.LinkParse.Enabled(),
			// 剧情分析页的「生成设置」：改过的值在 generate 时回传
			Models: rm, DefaultModel: defaultOf(l.ctx, l.svcCtx, genRemakeModels, rm[0].Code),
			Sizes: optionsOf(l.ctx, l.svcCtx, genVideoSizes, videoSizes), // 不选就跟原片，所以没有 default_size
		}, nil
	}
	cfg, err := videoKindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.VideoLimits
	return &pb.VideoOptionsResp{
		Models: optionsOf(l.ctx, l.svcCtx, genVideoModels, videoModels), DefaultModel: defaultOf(l.ctx, l.svcCtx, genVideoModels, cfg.DefaultModel),
		Durations: durationsOf(l.ctx, l.svcCtx, genVideoDurations, videoDurations), DefaultDuration: defaultDurationOf(l.ctx, l.svcCtx, genVideoDurations, int32(cfg.DefaultDuration)),
		Sizes: optionsOf(l.ctx, l.svcCtx, genVideoSizes, videoSizes), DefaultSize: defaultSizeOf(l.ctx, l.svcCtx, in.Kind, cfg.DefaultSize),
		MaxUserTextLen: int32(lim.MaxUserTextLen), MaxSellingPointsLen: int32(lim.MaxSellingPointsLen),
		PropRequired: in.Kind == model.KindShortDrama, // 短剧的商品图必填（87 要求）
	}, nil
}
