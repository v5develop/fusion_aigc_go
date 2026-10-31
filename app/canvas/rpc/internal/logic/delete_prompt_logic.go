package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeletePromptLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeletePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeletePromptLogic {
	return &DeletePromptLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeletePromptLogic) DeletePrompt(in *canvas.PromptIdReq) (*canvas.Empty, error) {
	return newCore(l.svcCtx).DeletePrompt(l.ctx, in)
}
