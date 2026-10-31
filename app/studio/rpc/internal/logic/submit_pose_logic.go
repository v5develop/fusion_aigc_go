package logic

import (
	"context"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitPoseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitPoseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitPoseLogic {
	return &SubmitPoseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 姿态复刻整批提交：人物 1 张 + 姿势 1–9 张，一个姿势一个子任务
func (l *SubmitPoseLogic) SubmitPose(in *studio.SubmitPoseReq) (*studio.SubmitResp, error) {
	v, err := ValidatePose(in, l.svcCtx.Config.ModelGroupMap())
	if err != nil {
		return nil, err
	}
	// source_sha1 需下载人物图才能算，由编排回填（repo.Batch.SetSourceSha1）
	return newDeps(l.svcCtx).submit(l.ctx, poseSpec(in, v))
}
