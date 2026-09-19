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
	if _, err := l.svcCtx.Human.FindOwned(l.ctx, in.Id, in.UserId); errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("数字人")
	} else if err != nil {
		return nil, err
	}
	return &pb.Empty{}, l.svcCtx.Human.SoftDelete(l.ctx, in.Id)
}
