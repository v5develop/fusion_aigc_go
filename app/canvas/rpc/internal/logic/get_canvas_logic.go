package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetCanvasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetCanvasLogic {
	return &GetCanvasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取画布：快照 + revision，运行态按每个节点最近一次 run 回填进 data
func (l *GetCanvasLogic) GetCanvas(in *canvas.CanvasIdReq) (*canvas.CanvasDoc, error) {
	return newCore(l.svcCtx).GetCanvas(l.ctx, in)
}
