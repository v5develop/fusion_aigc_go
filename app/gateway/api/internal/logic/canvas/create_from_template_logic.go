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

type CreateFromTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 从模板建画布（节点 / 连线 id 重新生成）
func NewCreateFromTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateFromTemplateLogic {
	return &CreateFromTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateFromTemplateLogic) CreateFromTemplate(req *types.CanvasFromTemplateReq) (resp *types.CanvasDetail, err error) {
	out, err := l.svcCtx.CanvasRpc.CreateCanvasFromTemplate(rpcCtx(l.ctx), &canvas_client.CreateCanvasFromTemplateReq{
		UserId: ctxdata.UserID(l.ctx), WorkspaceId: req.WorkspaceId, TemplateId: req.TemplateId, Title: req.Title})
	if err != nil {
		return nil, err
	}
	return toDetail(out), nil
}
