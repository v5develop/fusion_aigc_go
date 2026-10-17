package logic

import (
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetVideoLogic {
	return &GetVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetVideoLogic) GetVideo(in *pb.IdUserReq) (*pb.VideoInfo, error) {
	c, err := findOwnedVideo(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(c), nil
}
