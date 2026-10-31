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

type CreateTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 把画布存为我的模板
func NewCreateTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateTemplateLogic {
	return &CreateTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateTemplateLogic) CreateTemplate(req *types.CanvasTemplateCreateReq) (resp *types.CanvasTemplate, err error) {
	out, err := l.svcCtx.CanvasRpc.CreateTemplateFromCanvas(rpcCtx(l.ctx), &canvas_client.CreateTemplateFromCanvasReq{
		UserId: ctxdata.UserID(l.ctx), CanvasId: req.CanvasId, Title: req.Title, Description: req.Description})
	if err != nil {
		return nil, err
	}
	t := toTemplate(out)
	return &t, nil
}
