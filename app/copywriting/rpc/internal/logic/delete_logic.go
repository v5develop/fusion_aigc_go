package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/xerr"

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
	c, err := l.svcCtx.Copywriting.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("文案")
	}
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, l.svcCtx.Copywriting.SoftDelete(l.ctx, c.Id)
}
