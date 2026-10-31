package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveAsCanvasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveAsCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveAsCanvasLogic {
	return &SaveAsCanvasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *SaveAsCanvasLogic) SaveAsCanvas(in *canvas.SaveAsCanvasReq) (*canvas.CanvasDoc, error) {
	return newCore(l.svcCtx).SaveAsCanvas(l.ctx, in)
}
