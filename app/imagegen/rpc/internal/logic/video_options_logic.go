package logic

import (
	"context"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"

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
	cfg, err := videoKindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.VideoLimits
	return &pb.VideoOptionsResp{
		Models: videoModels, DefaultModel: cfg.DefaultModel,
		Durations: videoDurations, DefaultDuration: int32(cfg.DefaultDuration),
		Sizes: videoSizes, DefaultSize: cfg.DefaultSize,
		MaxUserTextLen: int32(lim.MaxUserTextLen), MaxSellingPointsLen: int32(lim.MaxSellingPointsLen),
		PropRequired: in.Kind == model.KindShortDrama, // 短剧的商品图必填（87 要求）
	}, nil
}
