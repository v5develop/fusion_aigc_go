package logic

import (
	"context"
	"errors"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/xerr"

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
	h, err := l.svcCtx.Human.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("数字人")
	}
	if err != nil {
		return nil, err
	}
	if err := l.svcCtx.Human.SoftDelete(l.ctx, in.Id); err != nil {
		return nil, err
	}
	// 素材库里的同一条一起删，两边保持一致
	if l.svcCtx.Config.Asset.Enabled {
		if err := l.svcCtx.Asset.SoftDeleteByTask(l.ctx, h.UserId, h.VideoTaskId.String); err != nil {
			l.Errorf("delete asset of digital human %d: %v", h.Id, err)
		}
	}
	return &pb.Empty{}, nil
}
