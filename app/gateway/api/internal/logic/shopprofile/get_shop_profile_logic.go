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

type GetShopProfileLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 档案详情（编辑页回填）
func NewGetShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetShopProfileLogic {
	return &GetShopProfileLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetShopProfileLogic) GetShopProfile(req *types.IdPath) (resp *types.ShopProfile, err error) {
	out, err := l.svcCtx.CopywritingRpc.GetShopProfile(l.ctx, &copywriting.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return toShopProfile(out), nil
}
