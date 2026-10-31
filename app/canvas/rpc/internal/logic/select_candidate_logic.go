package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SelectCandidateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSelectCandidateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SelectCandidateLogic {
	return &SelectCandidateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 选中候选：写回 run.selected_index（GET 画布回填 selectedCandidate 以它为准）
func (l *SelectCandidateLogic) SelectCandidate(in *canvas.SelectCandidateReq) (*canvas.RunResult, error) {
	return newCore(l.svcCtx).SelectCandidate(l.ctx, in)
}
