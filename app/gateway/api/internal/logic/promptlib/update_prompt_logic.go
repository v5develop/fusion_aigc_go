// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package promptlib

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdatePromptLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 修改我的提示词（官方 4002）
func NewUpdatePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdatePromptLogic {
	return &UpdatePromptLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdatePromptLogic) UpdatePrompt(req *types.PromptLibUpdateReq) (resp *types.PromptLibItem, err error) {
	out, err := l.svcCtx.CanvasRpc.UpdatePrompt(l.ctx, &canvas_client.UpsertPromptReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id, Title: req.Title,
		Prompt: req.Prompt, Models: req.Models, Tags: req.Tags})
	if err != nil {
		return nil, err
	}
	it := toItem(out)
	return &it, nil
}
