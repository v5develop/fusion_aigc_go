// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"aigc-local-life/pkg/ctxdata"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteAssetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除单个资产：不存在 / 不属于本人 4001，官方素材 2005「官方素材不可删除」
func NewDeleteAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteAssetLogic {
	return &DeleteAssetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteAssetLogic) DeleteAsset(req *types.IdPath) (resp *types.AssetLibDeleteResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.Delete(l.ctx, &assetlib_client.DeleteReq{UserId: ctxdata.UserID(l.ctx), AssetIds: []int64{req.Id}, Strict: true})
	if err != nil {
		return nil, err
	}
	return toDeleteResp(out), nil
}

func toDeleteResp(out *assetlib_client.DeleteResp) *types.AssetLibDeleteResp {
	ids := out.DeletedIds
	if ids == nil {
		ids = []int64{}
	}
	return &types.AssetLibDeleteResp{Deleted: out.Deleted, OfficialSkipped: out.OfficialSkipped, Missing: out.Missing, DeletedIds: ids}
}
