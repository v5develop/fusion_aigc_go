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

type RetryTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重试单个失败子任务（仅本人、任务失败且批次为全部失败 / 部分成功）：只重跑这张图的生图一步（沿用已存提示词，不再反推），按原入口重新冻结 1 张点数；返回批次详情（同 GET /batches/:id），之后照常轮询批次。重复点击幂等
func NewRetryTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryTaskLogic {
	return &RetryTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RetryTaskLogic) RetryTask(req *types.StudioTaskPath) (resp *types.StudioBatchDetail, err error) {
	out, err := l.svcCtx.StudioRpc.RetryTask(l.ctx, &studio_client.RetryTaskReq{TaskId: req.TaskId, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	d := toDetail(out)
	return &d, nil
}
