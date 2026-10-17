package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateShopProfileLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateShopProfileLogic {
	return &UpdateShopProfileLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *UpdateShopProfileLogic) UpdateShopProfile(in *pb.ShopProfileForm) (*pb.ShopProfile, error) {
	if _, err := findOwnedProfile(l.ctx, l.svcCtx, in.Id, in.UserId); err != nil {
		return nil, err
	}
	row, err := shopProfileForm(l.svcCtx.Config.ShopProfile, in)
	if err != nil {
		return nil, err
	}
	if err := checkText(l.ctx, l.svcCtx, "shop_profile", row.Name+"\n"+profileText(row)); err != nil {
		return nil, err
	}
	if err := l.svcCtx.ShopProfile.Save(l.ctx, row); err != nil {
		return nil, err
	}
	cur, err := findOwnedProfile(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toShopProfile(cur), nil
}
