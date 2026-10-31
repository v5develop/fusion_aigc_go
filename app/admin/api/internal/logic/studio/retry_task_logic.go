// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重做失败的子任务（task_id 为子任务号）
func NewRetryTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryTaskLogic {
	return &RetryTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RetryTaskLogic) RetryTask(req *types.StudioTaskPath) (resp *types.EmptyResp, err error) {
	operator := ctxdata.AdminID(l.ctx)
	if _, err := l.svcCtx.StudioRpc.AdminRetryTask(l.ctx, &studio_client.AdminTaskReq{TaskId: req.TaskId, OperatorId: operator}); err != nil {
		l.Errorw("admin retry studio task failed", logx.Field("task_id", req.TaskId), logx.Field("operator", operator), logx.Field("err", err.Error()))
		return nil, err
	}
	l.Infow("admin retry studio task", logx.Field("task_id", req.TaskId), logx.Field("operator", operator))
	return &types.EmptyResp{}, nil
}
