package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetPortraitLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetPortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPortraitLogic {
	return &GetPortraitLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetPortraitLogic) GetPortrait(in *pb.IdUserReq) (*pb.PortraitInfo, error) {
	p, err := l.svcCtx.Portrait.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("形象")
	}
	if err != nil {
		return nil, err
	}
	return toPortrait(p), nil
}
