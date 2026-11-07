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

type SubmitPoseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 姿态复刻整批提交（人物 1 张 + 姿势 1–9 张）；必须带 Idempotency-Key
func NewSubmitPoseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitPoseLogic {
	return &SubmitPoseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SubmitPoseLogic) SubmitPose(req *types.StudioPoseSubmitReq) (resp *types.StudioSubmitResp, err error) {
	prompts, err := posePrompts(req.PosePrompts)
	if err != nil {
		return nil, err
	}
	out, err := l.svcCtx.StudioRpc.SubmitPose(l.ctx, &studio_client.SubmitPoseReq{
		UserId: ctxdata.UserID(l.ctx), WorkspaceId: req.WorkspaceId, RequestId: req.RequestId,
		SourceImageUrl: req.SourceImageUrl, PoseImageUrls: req.PoseImageUrls, GlobalPrompt: req.GlobalPrompt,
		PosePrompts: prompts, Model: req.Model, Resolution: req.Resolution, AspectRatio: req.AspectRatio,
		SourcePersonCount: req.SourcePersonCount, Mappings: poseMappings(req.Mappings),
	})
	if err != nil {
		return nil, err
	}
	return toSubmitResp(out), nil
}
