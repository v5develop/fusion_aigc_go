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

type ListPromptsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提示词列表（默认官方）
func NewListPromptsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPromptsLogic {
	return &ListPromptsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListPromptsLogic) ListPrompts(req *types.PromptLibAdminListReq) (resp *types.PromptLibAdminListResp, err error) {
	out, err := l.svcCtx.CanvasRpc.AdminListPrompts(l.ctx, &canvas_client.AdminListPromptsReq{Page: req.Page, PageSize: req.PageSize, Scope: req.Scope,
		Vertical: req.Vertical, Group: req.Group, Enabled: req.Enabled, Q: req.Q})
	if err != nil {
		return nil, err
	}
	resp = &types.PromptLibAdminListResp{Total: out.Total, List: make([]types.PromptLibAdminItem, 0, len(out.List))}
	for _, p := range out.List {
		resp.List = append(resp.List, toItem(p))
	}
	return resp, nil
}
