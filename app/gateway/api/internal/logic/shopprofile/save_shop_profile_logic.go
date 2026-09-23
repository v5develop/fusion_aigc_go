// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package shopprofile

import (
	"context"

	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveShopProfileLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 保存店铺档案
func NewSaveShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveShopProfileLogic {
	return &SaveShopProfileLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SaveShopProfileLogic) SaveShopProfile(req *types.SaveShopProfileReq) (resp *types.ShopProfile, err error) {
	out, err := l.svcCtx.CopywritingRpc.UpdateShopProfile(l.ctx, &copywriting.ShopProfileForm{
		Id: req.Id, UserId: ctxdata.UserID(l.ctx), Name: req.Name, OpenDate: req.OpenDate, Address: req.Address,
		Products: req.Products, SellingPoints: req.SellingPoints, SelfTitle: req.SelfTitle,
		TargetAudience: req.TargetAudience,
	})
	if err != nil {
		return nil, err
	}
	return toShopProfile(out), nil
}
