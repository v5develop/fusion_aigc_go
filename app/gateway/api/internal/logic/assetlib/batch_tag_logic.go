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

type BatchTagLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量给资产加标签（追加）
func NewBatchTagLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchTagLogic {
	return &BatchTagLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *BatchTagLogic) BatchTag(req *types.AssetLibBatchTagReq) (resp *types.AssetLibBatchTagResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.BatchTag(l.ctx, &assetlib_client.BatchTagReq{UserId: ctxdata.UserID(l.ctx), AssetIds: req.AssetIds, TagIds: req.TagIds,
		RemoveTagIds: req.RemoveTagIds})
	if err != nil {
		return nil, err
	}
	return &types.AssetLibBatchTagResp{Added: out.Added, Removed: out.Removed}, nil

}
