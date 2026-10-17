// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package shopprofile

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateShopProfileLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 创建店铺档案
func NewCreateShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateShopProfileLogic {
	return &CreateShopProfileLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateShopProfileLogic) CreateShopProfile(req *types.ShopProfileForm) (resp *types.ShopProfile, err error) {
	out, err := l.svcCtx.CopywritingRpc.CreateShopProfile(l.ctx, &copywriting.ShopProfileForm{
		UserId: ctxdata.UserID(l.ctx), Name: req.Name, OpenDate: req.OpenDate, Address: req.Address,
		Products: req.Products, SellingPoints: req.SellingPoints, SelfTitle: req.SelfTitle,
		TargetAudience: req.TargetAudience,
	})
	if err != nil {
		return nil, err
	}
	return toShopProfile(out), nil
}
