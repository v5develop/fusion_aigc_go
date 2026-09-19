package logic

import (
	"context"

	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteLogic {
	return &DeleteLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteLogic) Delete(in *pb.IdUserReq) (*pb.Empty, error) {
	if _, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId); err != nil {
		return nil, err
	}
	return &pb.Empty{}, l.svcCtx.Note.SoftDelete(l.ctx, in.Id)
}
