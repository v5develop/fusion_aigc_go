// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryLibraryCopyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重新存入素材库：付费成品转存素材库永久失败（library_status=failed）时重新转存；仅本人；已在转存中 / 已在素材库时幂等返回当前状态；同一子任务每分钟至多一次（否则 4002）；返回该任务（同 GET /tasks/:task_id，library_status 变为 pending），之后照常轮询
func NewRetryLibraryCopyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryLibraryCopyLogic {
	return &RetryLibraryCopyLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RetryLibraryCopyLogic) RetryLibraryCopy(req *types.StudioTaskPath) (resp *types.StudioTask, err error) {
	out, err := l.svcCtx.StudioRpc.RetryLibraryCopy(l.ctx, &studio_client.GetTaskReq{TaskId: req.TaskId, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	t := toTask(out)
	return &t, nil
}
