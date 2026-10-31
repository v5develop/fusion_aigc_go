// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package promptlib

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type TogglePromptLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 官方提示词启停
func NewTogglePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TogglePromptLogic {
	return &TogglePromptLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *TogglePromptLogic) TogglePrompt(req *types.PromptLibAdminToggleReq) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.CanvasRpc.AdminTogglePrompt(l.ctx, &canvas_client.AdminToggleReq{Id: req.Id, Enabled: req.Enabled}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
