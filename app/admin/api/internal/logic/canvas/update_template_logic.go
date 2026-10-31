// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑官方模板
func NewUpdateTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateTemplateLogic {
	return &UpdateTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateTemplateLogic) UpdateTemplate(req *types.CanvasAdminTemplateUpsertReq) (resp *types.CanvasAdminTemplate, err error) {
	if req.Id <= 0 {
		return nil, xerr.Param("缺少模板 id")
	}
	out, err := l.svcCtx.CanvasRpc.AdminUpsertOfficialTemplate(l.ctx, &canvas_client.AdminUpsertTemplateReq{Id: req.Id, Title: req.Title,
		Description: req.Description, Snapshot: req.Snapshot, CoverUrl: req.CoverUrl, Sort: req.Sort, Enabled: req.Enabled})
	if err != nil {
		return nil, err
	}
	t := toTemplate(out)
	return &t, nil
}
