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

type CreateCanvasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建画布（空快照）
func NewCreateCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateCanvasLogic {
	return &CreateCanvasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateCanvasLogic) CreateCanvas(req *types.CanvasCreateReq) (resp *types.CanvasDetail, err error) {
	out, err := l.svcCtx.CanvasRpc.CreateCanvas(rpcCtx(l.ctx), &canvas_client.CreateCanvasReq{UserId: ctxdata.UserID(l.ctx), WorkspaceId: req.WorkspaceId, Title: req.Title})
	if err != nil {
		return nil, err
	}
	return toDetail(out), nil
}
