// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryLibraryCopyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重新存入素材库：成功 run 的产物转存素材库永久失败（library_status=failed）时重新转存；仅本人；已在转存中 / 已在素材库时幂等返回当前状态；同一 run 每分钟至多一次（否则 4002）；返回同 GET /runs/:id（library_status 变为 pending），之后照常轮询
func NewRetryLibraryCopyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryLibraryCopyLogic {
	return &RetryLibraryCopyLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RetryLibraryCopyLogic) RetryLibraryCopy(req *types.CanvasRunPath) (resp *types.CanvasRunResult, err error) {
	out, err := l.svcCtx.CanvasRpc.RetryLibraryCopy(rpcCtx(l.ctx), &canvas_client.RunIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id})
	if err != nil {
		return nil, err
	}
	r := toRunResult(out)
	return &r, nil
}
