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

type ForceFailTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 强制失败子任务：封禁重做，点数已退回不动账
func NewForceFailTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ForceFailTaskLogic {
	return &ForceFailTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ForceFailTaskLogic) ForceFailTask(req *types.StudioTaskPath) (resp *types.EmptyResp, err error) {
	operator := ctxdata.AdminID(l.ctx)
	if _, err := l.svcCtx.StudioRpc.AdminForceFail(l.ctx, &studio_client.AdminTaskReq{TaskId: req.TaskId, OperatorId: operator}); err != nil {
		l.Errorw("admin force-fail studio task failed", logx.Field("task_id", req.TaskId), logx.Field("operator", operator), logx.Field("err", err.Error()))
		return nil, err
	}
	l.Infow("admin force-fail studio task", logx.Field("task_id", req.TaskId), logx.Field("operator", operator))
	return &types.EmptyResp{}, nil
}
