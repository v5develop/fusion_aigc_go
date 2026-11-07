// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"
	"time"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
)

type PreviewPoseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 姿态复刻多人配对预览（只读、不计费）：返回原图与每张姿势图的人数、人物站位与默认配对；原图 ≥2 人时前端据此让用户确认 / 反转配对，再把 mappings 随提交带回
func NewPreviewPoseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PreviewPoseLogic {
	return &PreviewPoseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// previewCallTimeout 调 studio-rpc PreviewPose 的超时：比 rpc 服务端方法超时（240s）略长、比路由超时（250s）短，
// 网关 yaml 里 StudioRpc.Timeout（30s）只管其它方法。
const previewCallTimeout = 245 * time.Second

// msgPreviewTooFrequent 预览超出按用户配额（每分钟 / 每天，Config.Studio.PosePreview*）时的提示，码为 4002。
const msgPreviewTooFrequent = "识别太频繁，请稍后再试"

func (l *PreviewPoseLogic) PreviewPose(req *types.StudioPosePreviewReq) (resp *types.StudioPosePreviewResp, err error) {
	uid := ctxdata.UserID(l.ctx)
	// 预览免费但每次都要读图（占 sidecar），按用户限次（跨实例，计数在会话 Redis）；被拒的不计数
	if r := l.svcCtx.PosePreviewQuota.Take(l.ctx, uid); r != userquota.Allowed {
		l.Infow("studio pose preview quota exceeded", logx.Field("user_id", uid), logx.Field("window", quotaWindow(r)))
		return nil, xerr.New(xerr.Conflict, msgPreviewTooFrequent)
	}
	out, err := l.svcCtx.StudioRpc.PreviewPose(l.ctx, &studio_client.PreviewPoseReq{
		UserId: uid, SourceImageUrl: req.SourceImageUrl, PoseImageUrls: req.PoseImageUrls,
	}, zrpc.WithCallTimeout(previewCallTimeout))
	if err != nil {
		return nil, err
	}
	return toPosePreviewResp(out), nil
}

func quotaWindow(r userquota.Result) string {
	if r == userquota.DayExceed {
		return "day"
	}
	return "minute"
}
