// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package promptlib

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPromptsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提示词列表：官方（启用）+ 我的
func NewListPromptsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPromptsLogic {
	return &ListPromptsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListPromptsLogic) ListPrompts(req *types.PromptLibListReq) (resp *types.PromptLibListResp, err error) {
	out, err := l.svcCtx.CanvasRpc.ListPrompts(l.ctx, &canvas_client.ListPromptsReq{UserId: ctxdata.UserID(l.ctx), Vertical: req.Vertical,
		Group: req.Group, Model: req.Model, Q: req.Q, Page: req.Page, PageSize: req.PageSize})
	if err != nil {
		return nil, err
	}
	resp = &types.PromptLibListResp{Total: out.Total, Items: make([]types.PromptLibItem, 0, len(out.List)),
		Groups: make([]types.PromptLibGroup, 0, len(out.Groups))}
	for _, g := range out.Groups {
		resp.Groups = append(resp.Groups, types.PromptLibGroup{Vertical: g.Vertical, Key: g.Key, Label: g.Label, Count: g.Count})
	}
	if c := out.Counts; c != nil {
		resp.Counts = types.PromptLibCounts{Ecommerce: c.Ecommerce, General: c.General, Mine: c.Mine, OfficialTotal: c.OfficialTotal, MineTotal: c.MineTotal}
	}
	for _, p := range out.List {
		resp.Items = append(resp.Items, toItem(p))
	}
	return resp, nil
}
