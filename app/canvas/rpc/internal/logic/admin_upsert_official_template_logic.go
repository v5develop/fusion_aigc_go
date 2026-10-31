package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminUpsertOfficialTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminUpsertOfficialTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminUpsertOfficialTemplateLogic {
	return &AdminUpsertOfficialTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminUpsertOfficialTemplateLogic) AdminUpsertOfficialTemplate(in *canvas.AdminUpsertTemplateReq) (*canvas.TemplateInfo, error) {
	return newCore(l.svcCtx).AdminUpsertOfficialTemplate(l.ctx, in)
}
