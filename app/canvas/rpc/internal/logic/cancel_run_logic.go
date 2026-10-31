package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelRunLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelRunLogic {
	return &CancelRunLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CancelRunLogic) CancelRun(in *canvas.RunIdReq) (*canvas.RunResult, error) {
	return newCore(l.svcCtx).CancelRun(l.ctx, in)
}
