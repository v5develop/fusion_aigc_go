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

type CreateTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增官方模板
func NewCreateTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateTemplateLogic {
	return &CreateTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateTemplateLogic) CreateTemplate(req *types.CanvasAdminTemplateUpsertReq) (resp *types.CanvasAdminTemplate, err error) {
	req.Id = 0
	out, err := l.svcCtx.CanvasRpc.AdminUpsertOfficialTemplate(l.ctx, &canvas_client.AdminUpsertTemplateReq{Title: req.Title, Description: req.Description,
		Snapshot: req.Snapshot, CoverUrl: req.CoverUrl, Sort: req.Sort, Enabled: req.Enabled})
	if err != nil {
		return nil, err
	}
	t := toTemplate(out)
	return &t, nil
}
