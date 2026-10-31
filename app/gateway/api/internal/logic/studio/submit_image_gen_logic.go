// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitImageGenLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 图片生成（count 张拆任务）；必须带 Idempotency-Key
func NewSubmitImageGenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitImageGenLogic {
	return &SubmitImageGenLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SubmitImageGenLogic) SubmitImageGen(req *types.StudioImageGenSubmitReq) (resp *types.StudioSubmitResp, err error) {
	out, err := l.svcCtx.StudioRpc.SubmitImageGen(l.ctx, &studio_client.SubmitImageGenReq{
		UserId: ctxdata.UserID(l.ctx), WorkspaceId: req.WorkspaceId, RequestId: req.RequestId,
		Prompt: req.Prompt, ImageUrls: req.ImageUrls, Model: req.Model, Resolution: req.Resolution,
		AspectRatio: req.AspectRatio, Count: req.Count, RewritePrompt: req.RewritePrompt,
	})
	if err != nil {
		return nil, err
	}
	return toSubmitResp(out), nil
}
