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
	n, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if err := l.svcCtx.Note.SoftDelete(l.ctx, in.Id); err != nil {
		return nil, err
	}
	// 素材库里的同一组图一起删，两边保持一致
	if l.svcCtx.Config.Asset.Enabled {
		if err := l.svcCtx.Asset.SoftDeleteByTask(l.ctx, n.UserId, n.GenTaskId.String); err != nil {
			l.Errorf("delete assets of note %d: %v", n.Id, err)
		}
	}
	return &pb.Empty{}, nil
}
