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

type RemoveRelationLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除资产关系（reference 两个方向都删）
func NewRemoveRelationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemoveRelationLogic {
	return &RemoveRelationLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RemoveRelationLogic) RemoveRelation(req *types.AssetLibRelationDelReq) (resp *types.EmptyResp, err error) {
	_, err = l.svcCtx.AssetLibRpc.RemoveRelation(l.ctx, &assetlib_client.RelationReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx),
		RelatedAssetId: req.RelatedAssetId, RelationType: req.RelationType})
	if err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
