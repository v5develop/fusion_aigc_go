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

func (l *GetLogic) Get(in *pb.IdUserReq) (*pb.DigitalHumanInfo, error) {
	h, err := l.svcCtx.Human.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("数字人")
	}
	if err != nil {
		return nil, err
	}
	return toInfo(h, voiceNames(l.ctx, l.svcCtx, []*model.AfvDigitalHuman{h})), nil
}
