package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateCanvasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateCanvasLogic {
	return &CreateCanvasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreateCanvasLogic) CreateCanvas(in *canvas.CreateCanvasReq) (*canvas.CanvasDoc, error) {
	return newCore(l.svcCtx).CreateCanvas(l.ctx, in)
}
