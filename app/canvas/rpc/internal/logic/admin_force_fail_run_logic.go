package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminForceFailRunLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminForceFailRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminForceFailRunLogic {
	return &AdminForceFailRunLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminForceFailRunLogic) AdminForceFailRun(in *canvas.AdminRunReq) (*canvas.Empty, error) {
	return newCore(l.svcCtx).AdminForceFailRun(l.ctx, in)
}
