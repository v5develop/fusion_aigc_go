package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteTemplateLogic {
	return &DeleteTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteTemplateLogic) DeleteTemplate(in *canvas.TemplateIdReq) (*canvas.Empty, error) {
	return newCore(l.svcCtx).DeleteTemplate(l.ctx, in)
}
