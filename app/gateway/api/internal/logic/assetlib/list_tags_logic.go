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

type ListTagsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的标签字典（含每个标签的资产数）
func NewListTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTagsLogic {
	return &ListTagsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListTagsLogic) ListTags(req *types.AssetLibTagListReq) (resp *types.AssetLibTagListResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.ListTags(l.ctx, &assetlib_client.ListTagsReq{UserId: ctxdata.UserID(l.ctx), Sort: req.Sort})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibTagListResp{Items: make([]types.AssetLibTag, 0, len(out.Items)), UntaggedCount: out.UntaggedCount}
	for _, t := range out.Items {
		resp.Items = append(resp.Items, toTag(t))
	}
	return resp, nil
}
