package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTemplateLogic {
	return &GetTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetTemplateLogic) GetTemplate(in *canvas.TemplateIdReq) (*canvas.TemplateInfo, error) {
	return newCore(l.svcCtx).GetTemplate(l.ctx, in)
}
