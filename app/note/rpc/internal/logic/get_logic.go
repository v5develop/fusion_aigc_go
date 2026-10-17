package logic

import (
	"context"

	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/app/note/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLogic {
	return &GetLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetLogic) Get(in *pb.IdUserReq) (*pb.NoteInfo, error) {
	n, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toInfo(n), nil
}
