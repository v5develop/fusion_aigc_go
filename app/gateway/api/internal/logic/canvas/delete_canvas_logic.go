// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteCanvasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除（逻辑删除）
func NewDeleteCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteCanvasLogic {
	return &DeleteCanvasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteCanvasLogic) DeleteCanvas(req *types.CanvasIdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.CanvasRpc.DeleteCanvas(rpcCtx(l.ctx), &canvas_client.CanvasIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
