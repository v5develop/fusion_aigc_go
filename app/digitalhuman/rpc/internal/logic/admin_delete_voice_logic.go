package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminDeleteVoiceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminDeleteVoiceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminDeleteVoiceLogic {
	return &AdminDeleteVoiceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminDeleteVoiceLogic) AdminDeleteVoice(in *pb.IdReq) (*pb.Empty, error) {
	return &pb.Empty{}, l.svcCtx.Voice.SoftDelete(l.ctx, in.Id)
}
