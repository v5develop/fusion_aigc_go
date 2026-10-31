package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCanvasesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListCanvasesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCanvasesLogic {
	return &ListCanvasesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 画布 ----
func (l *ListCanvasesLogic) ListCanvases(in *canvas.ListCanvasesReq) (*canvas.ListCanvasesResp, error) {
	return newCore(l.svcCtx).ListCanvases(l.ctx, in)
}
