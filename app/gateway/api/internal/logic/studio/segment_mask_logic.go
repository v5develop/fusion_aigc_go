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

type SegmentMaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 区域蒙版（局部校色，免费）：识别图片的服装 / 商品 / 人物 / 背景区域，返回白色+软 alpha 的 PNG 蒙版（服装可带 parts 只取部分部位，另回逐件蒙版）；按（去查询串的地址, 区域）缓存，重复请求立即返回。按用户限频（超出 4002「识别太频繁，请稍后再试」）；没识别到 4001「没识别到对应区域」；识别服务不可用 6001
func NewSegmentMaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SegmentMaskLogic {
	return &SegmentMaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// maskCallTimeout 调 studio-rpc SegmentMask 的超时：rpc 侧识别总时限 120s + 余量，比路由超时（250s）短；
// 网关 yaml 里 StudioRpc.Timeout（30s）只管其它方法。
const maskCallTimeout = 135 * time.Second

func (l *SegmentMaskLogic) SegmentMask(req *types.StudioMaskReq) (resp *types.StudioMaskResp, err error) {
	uid := ctxdata.UserID(l.ctx)
	// 免费但未命中缓存时要调分割：按用户限次（跨实例，计数在会话 Redis）；被拒的不计数。文案与配对预览一致
	if r := l.svcCtx.MaskQuota.Take(l.ctx, uid); r != userquota.Allowed {
		l.Infow("studio mask quota exceeded", logx.Field("user_id", uid), logx.Field("window", quotaWindow(r)))
		return nil, xerr.New(xerr.Conflict, msgPreviewTooFrequent)
	}
	out, err := l.svcCtx.StudioRpc.SegmentMask(l.ctx, &studio_client.SegmentMaskReq{
		UserId: uid, ImageUrl: req.ImageUrl, Target: req.Target, Parts: req.Parts,
	}, zrpc.WithCallTimeout(maskCallTimeout))
	if err != nil {
		return nil, err
	}
	if out.Cached { // 命中缓存没有新的识别调用：不占额度
		l.svcCtx.MaskQuota.Refund(l.ctx, uid)
	}
	avail := append(make([]string, 0, len(out.AvailableParts)), out.AvailableParts...)
	return &types.StudioMaskResp{MaskUrl: out.MaskUrl, Target: out.Target, Cached: out.Cached,
		Parts: toMaskParts(out.Parts), AvailableParts: avail}, nil
}
