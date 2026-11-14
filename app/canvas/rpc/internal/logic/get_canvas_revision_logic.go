package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetCanvasRevisionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetCanvasRevisionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetCanvasRevisionLogic {
	return &GetCanvasRevisionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 画布当前 revision（只读元数据，不读快照）：网关 SSE 归属校验 / 心跳对账 / 降级轮询
func (l *GetCanvasRevisionLogic) GetCanvasRevision(in *canvas.CanvasIdReq) (*canvas.CanvasRevisionResp, error) {
	return newCore(l.svcCtx).GetCanvasRevision(l.ctx, in)
}
