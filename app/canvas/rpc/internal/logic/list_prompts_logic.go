package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPromptsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPromptsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPromptsLogic {
	return &ListPromptsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 提示词库 ----
func (l *ListPromptsLogic) ListPrompts(in *canvas.ListPromptsReq) (*canvas.ListPromptsResp, error) {
	return newCore(l.svcCtx).ListPrompts(l.ctx, in)
}
