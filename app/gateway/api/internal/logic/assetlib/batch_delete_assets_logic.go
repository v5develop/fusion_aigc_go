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

type BatchDeleteAssetsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量删除资产（逻辑删除 + 解除标签）：只删本人的上传 / 生成资产，官方素材与不存在的静默跳过并计数；不删存储对象、不影响任务结果
func NewBatchDeleteAssetsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchDeleteAssetsLogic {
	return &BatchDeleteAssetsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *BatchDeleteAssetsLogic) BatchDeleteAssets(req *types.AssetLibBatchDeleteReq) (resp *types.AssetLibDeleteResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.Delete(l.ctx, &assetlib_client.DeleteReq{UserId: ctxdata.UserID(l.ctx), AssetIds: req.AssetIds})
	if err != nil {
		return nil, err
	}
	return toDeleteResp(out), nil
}
