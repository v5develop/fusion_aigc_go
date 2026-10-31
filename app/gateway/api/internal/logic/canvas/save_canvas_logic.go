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

type SaveCanvasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 保存（乐观锁 if_match_revision，冲突 code=4002 data.conflict=revision；force=true 覆盖）
func NewSaveCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveCanvasLogic {
	return &SaveCanvasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SaveCanvasLogic) SaveCanvas(req *types.CanvasSaveReq) (resp *types.CanvasSaveResp, err error) {
	snap, err := snapshotText(req.Snapshot)
	if err != nil {
		return nil, err
	}
	in := &canvas_client.SaveCanvasReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id, Snapshot: snap, IfMatchRevision: req.IfMatchRevision, Force: req.Force}
	if req.Title != nil {
		in.HasTitle, in.Title = true, *req.Title
	}
	out, err := l.svcCtx.CanvasRpc.SaveCanvas(rpcCtx(l.ctx), in)
	if err != nil {
		return nil, err
	}
	if out.Conflict {
		return nil, &ConflictError{Revision: out.Revision, UpdatedAt: out.UpdatedAt}
	}
	return &types.CanvasSaveResp{Revision: out.Revision, UpdatedAt: out.UpdatedAt, NodeCount: out.NodeCount, CoverUrl: out.CoverUrl}, nil
}
