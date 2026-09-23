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

type RemoveShopProfileLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除店铺档案
func NewRemoveShopProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemoveShopProfileLogic {
	return &RemoveShopProfileLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RemoveShopProfileLogic) RemoveShopProfile(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err = l.svcCtx.CopywritingRpc.DeleteShopProfile(l.ctx, &copywriting.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
