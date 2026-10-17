package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminDeleteCopywritingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminDeleteCopywritingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminDeleteCopywritingLogic {
	return &AdminDeleteCopywritingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminDeleteCopywritingLogic) AdminDeleteCopywriting(in *pb.IdReq) (*pb.Empty, error) {
	return &pb.Empty{}, l.svcCtx.Copywriting.SoftDelete(l.ctx, in.Id)
}
