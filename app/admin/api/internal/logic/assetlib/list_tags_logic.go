// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTagsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 标签字典（全部用户，分页）
func NewListTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTagsLogic {
	return &ListTagsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListTagsLogic) ListTags(req *types.AssetLibAdminTagsReq) (resp *types.AssetLibAdminTagsResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.AdminListTags(l.ctx, &assetlib_client.AdminListTagsReq{Page: req.Page, PageSize: req.PageSize, UserId: req.UserId, Q: req.Q})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibAdminTagsResp{List: make([]types.AssetLibAdminTag, 0, len(out.Items)), Total: out.Total}
	for _, t := range out.Items {
		resp.List = append(resp.List, types.AssetLibAdminTag{Id: t.Id, UserId: t.UserId, Name: t.Name, AssetCount: t.AssetCount, CreatedAt: t.CreatedAt})
	}
	return resp, nil
}
