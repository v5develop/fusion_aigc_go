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

type ListShopProfilesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 店铺档案列表
func NewListShopProfilesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListShopProfilesLogic {
	return &ListShopProfilesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListShopProfilesLogic) ListShopProfiles(req *types.ShopProfileListReq) (resp *types.ShopProfileListResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.ListShopProfiles(l.ctx, &copywriting.ShopProfileListReq{
		UserId: ctxdata.UserID(l.ctx), Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.ShopProfileListResp{Total: out.Total, List: make([]types.ShopProfile, 0, len(out.List))}
	for _, p := range out.List {
		resp.List = append(resp.List, *toShopProfile(p))
	}
	return resp, nil
}
