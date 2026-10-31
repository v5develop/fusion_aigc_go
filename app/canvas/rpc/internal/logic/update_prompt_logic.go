package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdatePromptLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdatePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdatePromptLogic {
	return &UpdatePromptLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *UpdatePromptLogic) UpdatePrompt(in *canvas.UpsertPromptReq) (*canvas.PromptItem, error) {
	return newCore(l.svcCtx).UpdatePrompt(l.ctx, in)
}
