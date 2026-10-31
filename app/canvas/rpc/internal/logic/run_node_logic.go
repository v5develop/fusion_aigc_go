package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunNodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunNodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunNodeLogic {
	return &RunNodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 执行 ----
func (l *RunNodeLogic) RunNode(in *canvas.RunNodeReq) (*canvas.RunNodeResp, error) {
	return newCore(l.svcCtx).RunNode(l.ctx, in)
}
