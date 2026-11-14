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

type GetCanvasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取画布：快照 + revision，运行态按最近一次 run 回填
func NewGetCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetCanvasLogic {
	return &GetCanvasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetCanvasLogic) GetCanvas(req *types.CanvasIdPath) (resp *types.CanvasDetail, err error) {
	return l.GetCanvasAt(req, "")
}

// GetCanvasAt revision 非空且不是当前版本时取历史快照（canvas-rpc 只保留最近 Limits.RevisionKeep 个）。
func (l *GetCanvasLogic) GetCanvasAt(req *types.CanvasIdPath, revision string) (resp *types.CanvasDetail, err error) {
	out, err := l.svcCtx.CanvasRpc.GetCanvas(rpcCtx(l.ctx), &canvas_client.CanvasIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id, Revision: revision})
	if err != nil {
		return nil, err
	}
	return toDetail(out), nil
}
