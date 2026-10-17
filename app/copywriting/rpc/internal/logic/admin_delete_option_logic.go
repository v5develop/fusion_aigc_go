package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminDeleteOptionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminDeleteOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminDeleteOptionLogic {
	return &AdminDeleteOptionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminDeleteOptionLogic) AdminDeleteOption(in *pb.IdReq) (*pb.Empty, error) {
	return &pb.Empty{}, l.svcCtx.Option.SoftDelete(l.ctx, in.Id)
}
