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

type SelectCandidateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 选中候选：写回 run.selected_index，刷新后 selectedCandidate 仍是它
func NewSelectCandidateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SelectCandidateLogic {
	return &SelectCandidateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SelectCandidateLogic) SelectCandidate(req *types.CanvasSelectCandidateReq) (resp *types.CanvasRunResult, err error) {
	out, err := l.svcCtx.CanvasRpc.SelectCandidate(rpcCtx(l.ctx), &canvas_client.SelectCandidateReq{
		UserId: ctxdata.UserID(l.ctx), CanvasId: req.Id, NodeId: req.NodeId, RunId: req.RunId, Index: req.Index,
	})
	if err != nil {
		return nil, err
	}
	r := toRunResult(out)
	return &r, nil
}
