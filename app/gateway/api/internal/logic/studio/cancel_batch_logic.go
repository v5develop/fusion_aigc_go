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

type CancelBatchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消批次：停止跟踪，未开始的任务退款
func NewCancelBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelBatchLogic {
	return &CancelBatchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CancelBatchLogic) CancelBatch(req *types.IdPath) (resp *types.StudioCancelResp, err error) {
	out, err := l.svcCtx.StudioRpc.CancelBatch(l.ctx, &studio_client.BatchUserReq{BatchId: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return &types.StudioCancelResp{Batch: toBatch(out.Batch), CanceledTasks: out.CanceledTasks}, nil
}
