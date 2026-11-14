// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSysTagsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 系统标签字典：本人资产里出现过的系统标签及资产数（按数量倒序，最多 500 个）
func NewListSysTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSysTagsLogic {
	return &ListSysTagsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListSysTagsLogic) ListSysTags() (resp *types.AssetLibSysTagListResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.ListSysTags(l.ctx, &assetlib_client.UserReq{UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibSysTagListResp{Items: make([]types.AssetLibSysTag, 0, len(out.Items))}
	for _, t := range out.Items {
		resp.Items = append(resp.Items, types.AssetLibSysTag{Name: t.Name, AssetCount: t.AssetCount})
	}
	return resp, nil
}
