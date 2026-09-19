package logic

import (
	"context"

	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListLogic) List(in *pb.ListReq) (*pb.NoteList, error) {
	return list(l.ctx, l.svcCtx, in)
}
