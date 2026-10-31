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

type GetTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模板详情
func NewGetTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTemplateLogic {
	return &GetTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetTemplateLogic) GetTemplate(req *types.IdPath) (resp *types.CanvasTemplate, err error) {
	out, err := l.svcCtx.CanvasRpc.GetTemplate(rpcCtx(l.ctx), &canvas_client.TemplateIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id})
	if err != nil {
		return nil, err
	}
	t := toTemplate(out)
	return &t, nil
}
