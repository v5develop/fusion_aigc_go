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

type CreatePromptLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增官方提示词
func NewCreatePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePromptLogic {
	return &CreatePromptLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreatePromptLogic) CreatePrompt(req *types.PromptLibAdminUpsertReq) (resp *types.PromptLibAdminItem, err error) {
	out, err := l.svcCtx.CanvasRpc.AdminUpsertOfficialPrompt(l.ctx, &canvas_client.AdminUpsertPromptReq{Title: req.Title, Prompt: req.Prompt,
		Vertical: req.Vertical, Group: req.Group, Models: req.Models, Tags: req.Tags, Source: req.Source, Sort: req.Sort, Enabled: req.Enabled})
	if err != nil {
		return nil, err
	}
	it := toItem(out)
	return &it, nil
}
