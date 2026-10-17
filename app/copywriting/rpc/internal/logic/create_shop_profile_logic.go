package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateShopProfileLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateShopProfileLogic {
	return &CreateShopProfileLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 店铺档案（内容生产的数据底座，一个账号可建多条）----
func (l *CreateShopProfileLogic) CreateShopProfile(in *pb.ShopProfileForm) (*pb.ShopProfile, error) {
	cfg := l.svcCtx.Config.ShopProfile
	n, err := l.svcCtx.ShopProfile.CountByUser(l.ctx, in.UserId)
	if err != nil {
		return nil, err
	}
	if int(n) >= cfg.MaxCount {
		return nil, xerr.Newf(xerr.ParamError, "最多创建 %d 条店铺档案，请先删除不用的", cfg.MaxCount)
	}
	row, err := shopProfileForm(cfg, in)
	if err != nil {
		return nil, err
	}
	if err := checkText(l.ctx, l.svcCtx, "shop_profile", row.Name+"\n"+profileText(row)); err != nil {
		return nil, err
	}
	res, err := l.svcCtx.ShopProfile.Insert(l.ctx, row)
	if err != nil {
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	cur, err := findOwnedProfile(l.ctx, l.svcCtx, row.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toShopProfile(cur), nil
}
