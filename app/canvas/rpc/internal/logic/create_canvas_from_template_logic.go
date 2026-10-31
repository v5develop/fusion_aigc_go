package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateCanvasFromTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateCanvasFromTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateCanvasFromTemplateLogic {
	return &CreateCanvasFromTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreateCanvasFromTemplateLogic) CreateCanvasFromTemplate(in *canvas.CreateCanvasFromTemplateReq) (*canvas.CanvasDoc, error) {
	return newCore(l.svcCtx).CreateCanvasFromTemplate(l.ctx, in)
}
