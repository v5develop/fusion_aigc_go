package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListTemplatesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListTemplatesLogic {
	return &AdminListTemplatesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminListTemplatesLogic) AdminListTemplates(in *canvas.AdminListTemplatesReq) (*canvas.AdminListTemplatesResp, error) {
	return newCore(l.svcCtx).AdminListTemplates(l.ctx, in)
}
