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

type RegenerateTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑提示词后重新生成单张（仅姿态复刻、仅本人；失败或成功的都可以）：用改过的提示词重跑这张图的生图一步，计费同重试（重新冻结 1 张点数）；同一提示词重复提交幂等；返回批次详情（同 GET /batches/:id）
func NewRegenerateTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegenerateTaskLogic {
	return &RegenerateTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RegenerateTaskLogic) RegenerateTask(req *types.StudioRegenerateReq) (resp *types.StudioBatchDetail, err error) {
	out, err := l.svcCtx.StudioRpc.RegenerateTask(l.ctx, &studio_client.RegenerateTaskReq{
		TaskId: req.TaskId, UserId: ctxdata.UserID(l.ctx), Prompt: req.Prompt,
	})
	if err != nil {
		return nil, err
	}
	d := toDetail(out)
	return &d, nil
}
