package logic

import (
	"context"
	"errors"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteActionVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteActionVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteActionVideoLogic {
	return &DeleteActionVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteActionVideoLogic) DeleteActionVideo(in *pb.IdUserReq) (*pb.Empty, error) {
	if _, err := l.svcCtx.ActionVideo.FindOwned(l.ctx, in.Id, in.UserId); errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("动作视频")
	} else if err != nil {
		return nil, err
	}
	return &pb.Empty{}, l.svcCtx.ActionVideo.SoftDelete(l.ctx, in.Id)
}
