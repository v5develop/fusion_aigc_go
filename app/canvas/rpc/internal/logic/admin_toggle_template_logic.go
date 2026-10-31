package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminToggleTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminToggleTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminToggleTemplateLogic {
	return &AdminToggleTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminToggleTemplateLogic) AdminToggleTemplate(in *canvas.AdminToggleReq) (*canvas.Empty, error) {
	return newCore(l.svcCtx).AdminToggleTemplate(l.ctx, in)
}
