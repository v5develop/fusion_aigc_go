package logic

import (
	"context"

	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/app/note/rpc/pb"

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
func (l *OptionsLogic) Options(in *pb.Empty) (*pb.OptionsResp, error) {
	lim := l.svcCtx.Config.Limits
	return &pb.OptionsResp{
		Types: noteTypes, MaxMaterials: int32(lim.MaxMaterials), DefaultImages: int32(lim.DefaultImages),
		MaxEditImages: int32(lim.MaxEditImages), MaxTopicLen: int32(lim.MaxTopicLen),
	}, nil
}
