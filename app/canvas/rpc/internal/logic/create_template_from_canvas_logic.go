package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateTemplateFromCanvasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateTemplateFromCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateTemplateFromCanvasLogic {
	return &CreateTemplateFromCanvasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreateTemplateFromCanvasLogic) CreateTemplateFromCanvas(in *canvas.CreateTemplateFromCanvasReq) (*canvas.TemplateInfo, error) {
	return newCore(l.svcCtx).CreateTemplateFromCanvas(l.ctx, in)
}
