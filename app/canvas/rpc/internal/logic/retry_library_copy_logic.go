package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryLibraryCopyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryLibraryCopyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryLibraryCopyLogic {
	return &RetryLibraryCopyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 重新存入素材库：成功 run 的产物转存永久失败（library_status=failed）时把转存任务重置为待转存并立即执行；
func (l *RetryLibraryCopyLogic) RetryLibraryCopy(in *canvas.RunIdReq) (*canvas.RunResult, error) {
	return newCore(l.svcCtx).RetryLibraryCopy(l.ctx, in)
}
