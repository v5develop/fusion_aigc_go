package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminUpsertOfficialPromptLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminUpsertOfficialPromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminUpsertOfficialPromptLogic {
	return &AdminUpsertOfficialPromptLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminUpsertOfficialPromptLogic) AdminUpsertOfficialPrompt(in *canvas.AdminUpsertPromptReq) (*canvas.PromptItem, error) {
	return newCore(l.svcCtx).AdminUpsertOfficialPrompt(l.ctx, in)
}
