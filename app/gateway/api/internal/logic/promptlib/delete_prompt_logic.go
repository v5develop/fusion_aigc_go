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

type DeletePromptLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除我的提示词（官方 4002）
func NewDeletePromptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeletePromptLogic {
	return &DeletePromptLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeletePromptLogic) DeletePrompt(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.CanvasRpc.DeletePrompt(l.ctx, &canvas_client.PromptIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
