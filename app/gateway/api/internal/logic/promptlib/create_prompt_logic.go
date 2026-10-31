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

type CreatePromptLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增我的提示词（每人 ≤500 条）
func NewCreatePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePromptLogic {
	return &CreatePromptLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreatePromptLogic) CreatePrompt(req *types.PromptLibUpsertReq) (resp *types.PromptLibItem, err error) {
	out, err := l.svcCtx.CanvasRpc.CreatePrompt(l.ctx, &canvas_client.UpsertPromptReq{UserId: ctxdata.UserID(l.ctx), Title: req.Title,
		Prompt: req.Prompt, Models: req.Models, Tags: req.Tags})
	if err != nil {
		return nil, err
	}
	it := toItem(out)
	return &it, nil
}
