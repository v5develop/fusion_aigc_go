// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTemplatesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模板列表（官方 + 用户）
func NewListTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTemplatesLogic {
	return &ListTemplatesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListTemplatesLogic) ListTemplates(req *types.CanvasAdminTemplatesReq) (resp *types.CanvasAdminTemplatesResp, err error) {
	out, err := l.svcCtx.CanvasRpc.AdminListTemplates(l.ctx, &canvas_client.AdminListTemplatesReq{Page: req.Page, PageSize: req.PageSize,
		Scope: req.Scope, Enabled: req.Enabled, Q: req.Q})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasAdminTemplatesResp{Total: out.Total, List: make([]types.CanvasAdminTemplate, 0, len(out.List))}
	for _, t := range out.List {
		resp.List = append(resp.List, toTemplate(t))
	}
	return resp, nil
}
