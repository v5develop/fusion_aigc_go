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

type ListTemplatesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模板列表（official / user / all）
func NewListTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTemplatesLogic {
	return &ListTemplatesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListTemplatesLogic) ListTemplates(req *types.CanvasTemplateListReq) (resp *types.CanvasTemplateListResp, err error) {
	out, err := l.svcCtx.CanvasRpc.ListTemplates(rpcCtx(l.ctx), &canvas_client.ListTemplatesReq{UserId: ctxdata.UserID(l.ctx), Scope: req.Scope})
	if err != nil {
		return nil, err
	}
	resp = &types.CanvasTemplateListResp{Items: make([]types.CanvasTemplate, 0, len(out.List))}
	for _, t := range out.List {
		resp.Items = append(resp.Items, toTemplate(t))
	}
	return resp, nil
}
