package logic

import (
	"context"

	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetShopProfileLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetShopProfileLogic {
	return &GetShopProfileLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetShopProfileLogic) GetShopProfile(in *pb.IdUserReq) (*pb.ShopProfile, error) {
	p, err := findOwnedProfile(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toShopProfile(p), nil
}
