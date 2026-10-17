package logic

import (
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

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
	c, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if err := l.svcCtx.Creation.SoftDelete(l.ctx, in.Id); err != nil {
		return nil, err
	}
	// 还在生成中就删了：成品用户拿不到，把冻结的点数退回（已结算的单这里是空操作）
	l.svcCtx.Billing.Fail(l.ctx, c.UserId, c.RequestId.String, "用户删除了生成中的任务")
	// 素材库里的同一组一起删，两边保持一致
	if l.svcCtx.Config.Asset.Enabled {
		if err := l.svcCtx.Asset.SoftDeleteByTask(l.ctx, c.UserId, groupTaskID(c)); err != nil {
			l.Errorf("delete assets of %s %d: %v", c.Kind, c.Id, err)
		}
	}
	return &pb.Empty{}, nil
}
