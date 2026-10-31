package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListPromptsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListPromptsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListPromptsLogic {
	return &AdminListPromptsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminListPromptsLogic) AdminListPrompts(in *canvas.AdminListPromptsReq) (*canvas.AdminListPromptsResp, error) {
	return newCore(l.svcCtx).AdminListPrompts(l.ctx, in)
}
