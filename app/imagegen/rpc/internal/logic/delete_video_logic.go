package logic

import (
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteVideoLogic {
	return &DeleteVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteVideoLogic) DeleteVideo(in *pb.IdUserReq) (*pb.Empty, error) {
	c, err := findOwnedVideo(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if err := l.svcCtx.Video.SoftDelete(l.ctx, in.Id); err != nil {
		return nil, err
	}
	// 还在生成中就删了：成品用户拿不到，把出片时冻结的点数退回（已结算的单这里是空操作）
	if c.GenTaskId.Valid {
		l.svcCtx.Billing.Fail(l.ctx, c.UserId, c.GenTaskId.String, "用户删除了生成中的任务")
	}
	// 成品同步从素材库删除；生成中删除后轮询器不再跟进（执行面的任务没有取消接口，会自己跑完）
	if c.GenTaskId.Valid && l.svcCtx.Config.Asset.Enabled {
		if err := l.svcCtx.Asset.SoftDeleteByTask(l.ctx, c.UserId, c.GenTaskId.String); err != nil {
			l.Errorf("delete assets of %s %d: %v", c.Kind, c.Id, err)
		}
	}
	return &pb.Empty{}, nil
}
