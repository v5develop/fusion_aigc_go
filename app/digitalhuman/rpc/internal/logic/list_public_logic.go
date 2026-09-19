package logic

import (
	"context"

	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPublicLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPublicLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPublicLogic {
	return &ListPublicLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListPublicLogic) ListPublic(in *pb.Empty) (*pb.PublicList, error) {
	list, err := l.svcCtx.Shared.ListPublicModels(l.ctx, publicSlot(l.svcCtx))
	if err != nil {
		return nil, err
	}
	resp := &pb.PublicList{List: make([]*pb.PublicInfo, 0, len(list))}
	for _, p := range list {
		resp.List = append(resp.List, toPublic(p))
	}
	return resp, nil
}
