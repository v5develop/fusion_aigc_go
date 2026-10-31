package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRunsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRunsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRunsLogic {
	return &ListRunsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListRunsLogic) ListRuns(in *canvas.ListRunsReq) (*canvas.ListRunsResp, error) {
	return newCore(l.svcCtx).ListRuns(l.ctx, in)
}
