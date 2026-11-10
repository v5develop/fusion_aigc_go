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

type DetectDetailsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 商拍「AI 找细节」（免费、不建任务）：对服装图找细节，返回每张图的细节框（归一化 [x,y,w,h]）与部位名；前端按框在原图上显示裁切缩略图，删减 / 重新裁切后随提交带回 detail_crops。按用户限频（超出 4002「识别太频繁，请稍后再试」）；上一次还没结束同为 4002（「上一次 AI 找细节还在进行中」）
func NewDetectDetailsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DetectDetailsLogic {
	return &DetectDetailsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// detailsCallTimeout 调 studio-rpc DetectDetails 的超时：一次 crop-plan（服装图 >6 张时分批，单次读图最长约 180s）+ 下载压缩，
// 与配对预览同口径（比 rpc 服务端方法超时略长、比路由超时 250s 短）。
const detailsCallTimeout = 245 * time.Second

func (l *DetectDetailsLogic) DetectDetails(req *types.StudioDetailsReq) (resp *types.StudioDetailsResp, err error) {
	uid := ctxdata.UserID(l.ctx)
	// 免费但每次都要读图（占 sidecar），按用户限次（跨实例，计数在会话 Redis）；被拒的不计数。同一用户并发由 rpc 侧单飞拦截
	if r := l.svcCtx.DetailsQuota.Take(l.ctx, uid); r != userquota.Allowed {
		l.Infow("studio detect details quota exceeded", logx.Field("user_id", uid), logx.Field("window", quotaWindow(r)))
		return nil, xerr.New(xerr.Conflict, msgPreviewTooFrequent)
	}
	garments := make([]*studio_client.GarmentImage, 0, len(req.GarmentImages))
	for _, g := range req.GarmentImages {
		garments = append(garments, &studio_client.GarmentImage{Url: g.Url, Role: g.Role, Angle: g.Angle})
	}
	out, err := l.svcCtx.StudioRpc.DetectDetails(l.ctx, &studio_client.DetectDetailsReq{
		UserId: uid, Items: req.Items, SetMode: req.SetMode, GarmentImages: garments, Fabric: req.Fabric, Prompt: req.Prompt,
	}, zrpc.WithCallTimeout(detailsCallTimeout))
	if err != nil {
		return nil, err
	}
	return toDetailsResp(out), nil
}
