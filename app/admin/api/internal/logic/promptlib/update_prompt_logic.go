// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package promptlib

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdatePromptLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑官方提示词
func NewUpdatePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdatePromptLogic {
	return &UpdatePromptLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdatePromptLogic) UpdatePrompt(req *types.PromptLibAdminUpsertReq) (resp *types.PromptLibAdminItem, err error) {
	if req.Id <= 0 {
		return nil, xerr.Param("缺少提示词 id")
	}
	out, err := l.svcCtx.CanvasRpc.AdminUpsertOfficialPrompt(l.ctx, &canvas_client.AdminUpsertPromptReq{Id: req.Id, Title: req.Title, Prompt: req.Prompt,
		Vertical: req.Vertical, Group: req.Group, Models: req.Models, Tags: req.Tags, Source: req.Source, Sort: req.Sort, Enabled: req.Enabled})
	if err != nil {
		return nil, err
	}
	it := toItem(out)
	return &it, nil
}
