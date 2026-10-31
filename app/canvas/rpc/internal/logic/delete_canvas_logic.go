package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteCanvasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteCanvasLogic {
	return &DeleteCanvasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteCanvasLogic) DeleteCanvas(in *canvas.CanvasIdReq) (*canvas.Empty, error) {
	return newCore(l.svcCtx).DeleteCanvas(l.ctx, in)
}
