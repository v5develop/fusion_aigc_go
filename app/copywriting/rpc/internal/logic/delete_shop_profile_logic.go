package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteShopProfileLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteShopProfileLogic {
	return &DeleteShopProfileLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteShopProfileLogic) DeleteShopProfile(in *pb.IdUserReq) (*pb.Empty, error) {
	if _, err := findOwnedProfile(l.ctx, l.svcCtx, in.Id, in.UserId); err != nil {
		return nil, err
	}
	// 逻辑删除：文案、笔记等历史记录里存的 shop_profile_id 不受影响（提示词在生成时已拼好）
	if err := l.svcCtx.ShopProfile.SoftDelete(l.ctx, in.Id); err != nil {
		return nil, err
	}
	return &pb.Empty{}, nil
}
