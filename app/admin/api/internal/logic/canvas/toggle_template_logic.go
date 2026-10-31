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

type ToggleTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模板上下架
func NewToggleTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ToggleTemplateLogic {
	return &ToggleTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ToggleTemplateLogic) ToggleTemplate(req *types.CanvasAdminToggleReq) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.CanvasRpc.AdminToggleTemplate(l.ctx, &canvas_client.AdminToggleReq{Id: req.Id, Enabled: req.Enabled}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
