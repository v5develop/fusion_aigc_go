// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteBatchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除批次（生成中不能删）
func NewDeleteBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteBatchLogic {
	return &DeleteBatchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteBatchLogic) DeleteBatch(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.StudioRpc.DeleteBatch(l.ctx, &studio_client.BatchUserReq{BatchId: req.Id, UserId: ctxdata.UserID(l.ctx)}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
