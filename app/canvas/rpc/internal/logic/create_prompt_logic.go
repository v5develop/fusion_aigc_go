package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreatePromptLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreatePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePromptLogic {
	return &CreatePromptLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreatePromptLogic) CreatePrompt(in *canvas.UpsertPromptReq) (*canvas.PromptItem, error) {
	return newCore(l.svcCtx).CreatePrompt(l.ctx, in)
}
