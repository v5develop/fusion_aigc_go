// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/rpc/canvas_client"
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
	in, err := l.maskSource(uid, req)
	if err != nil {
		l.svcCtx.MaskQuota.Refund(l.ctx, uid) // 来源不合法没有识别调用：不占额度
		return nil, err
	}
	out, err := l.svcCtx.StudioRpc.SegmentMask(l.ctx, in, zrpc.WithCallTimeout(maskCallTimeout))
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

// maskSource 组 SegmentMask 请求。图片来源（只增，2026-11-11）：task_id（studio 子任务，studio-rpc 自己按归属取成品）或
// canvas_request_id + candidate_index（画布候选：先调 canvas-rpc CandidateSource 按 run 归属取候选，私有引用给 rid、
// 填进只有网关会填的 source_rid；公开地址作 image_url），都不给按 image_url（老前端）。前端给的 source_rid 不存在这个入口。
func (l *SegmentMaskLogic) maskSource(uid int64, req *types.StudioMaskReq) (*studio_client.SegmentMaskReq, error) {
	in := &studio_client.SegmentMaskReq{UserId: uid, ImageUrl: strings.TrimSpace(req.ImageUrl), Target: req.Target, Parts: req.Parts,
		TaskId: strings.TrimSpace(req.TaskId)}
	reqID := strings.TrimSpace(req.CanvasRequestId)
	if reqID == "" {
		if req.CandidateIndex != nil {
			return nil, xerr.Param("candidate_index 需配合 canvas_request_id")
		}
		if in.ImageUrl == "" && in.TaskId == "" {
			return nil, xerr.New(xerr.ParamMissing, "请提供图片地址")
		}
		return in, nil
	}
	if in.TaskId != "" {
		return nil, xerr.Param("task_id 与 canvas_request_id 只能给一个")
	}
	id, err := strconv.ParseInt(reqID, 10, 64)
	if err != nil || id <= 0 {
		return nil, xerr.Param("canvas_request_id 不合法")
	}
	if l.svcCtx.CanvasRpc == nil {
		return nil, xerr.New(xerr.NotReady, "画布暂未开放")
	}
	src, err := l.svcCtx.CanvasRpc.CandidateSource(l.ctx, &canvas_client.CandidateSourceReq{UserId: uid, Id: id, CandidateIndex: req.CandidateIndex})
	if err != nil {
		return nil, maskSourceErr(err)
	}
	if src.Rid != "" {
		// 私有候选：image_url 只是前端展示用的签名地址，不再参与判断（来源已由 canvas-rpc 按归属确定）
		in.SourceRid, in.ImageUrl = src.Rid, ""
		return in, nil
	}
	if src.Url == "" {
		return nil, xerr.Param("这张图片暂时不可用")
	}
	in.ImageUrl = src.Url // 公开候选：按落库的长期地址走原口径（本服务存储 / 素材库白名单）
	return in, nil
}

// maskSourceErr 把画布来源错误换成蒙版专属 reason（码不变）：4001 → MASK_SOURCE_NOT_FOUND，4002 → MASK_SOURCE_NOT_READY；
// 其余（1003 非图片 / 下标不合法等）原样返回。与「没识别到区域」（4001 RESOURCE_NOT_FOUND）、「识别太频繁」（4002 RESOURCE_CONFLICT）区分。
func maskSourceErr(err error) error {
	switch code, _, _ := xerr.Parse(err); code {
	case xerr.NotFound:
		return xerr.WithReason(err, xerr.ReasonMaskSourceNotFound)
	case xerr.Conflict:
		return xerr.WithReason(err, xerr.ReasonMaskSourceNotReady)
	}
	return err
}
