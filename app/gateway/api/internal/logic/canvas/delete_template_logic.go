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

type DeleteTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除我的模板（官方模板不可删）
func NewDeleteTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteTemplateLogic {
	return &DeleteTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteTemplateLogic) DeleteTemplate(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.CanvasRpc.DeleteTemplate(rpcCtx(l.ctx), &canvas_client.TemplateIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
