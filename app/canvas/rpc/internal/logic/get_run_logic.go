package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRunLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRunLogic {
	return &GetRunLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetRunLogic) GetRun(in *canvas.RunIdReq) (*canvas.RunResult, error) {
	return newCore(l.svcCtx).GetRun(l.ctx, in)
}
