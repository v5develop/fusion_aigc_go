package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type SegmentMaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSegmentMaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SegmentMaskLogic {
	return &SegmentMaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 区域蒙版（局部校色，免费）：image_url 的 garment / product / person / background 区域 → 白色+软 alpha PNG（素材库桶公开地址）；
// garment 可带 parts 只取这些部位（合并蒙版 + 逐件蒙版）。
func (l *SegmentMaskLogic) SegmentMask(in *studio.SegmentMaskReq) (*studio.SegmentMaskResp, error) {
	return segmentMask(l.ctx, newMasker(l.svcCtx), newDeps(l.svcCtx).owns, in)
}

// segmentMask 校验入参与地址来源后取蒙版。地址只认本存储签发或白名单前缀（素材库域名）：不替任意第三方地址下载。
func segmentMask(ctx context.Context, m *masker, owns func(string) bool, in *studio.SegmentMaskReq) (*studio.SegmentMaskResp, error) {
	if in.GetUserId() <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	u := strings.TrimSpace(in.GetImageUrl())
	if u == "" {
		return nil, xerr.New(xerr.ParamMissing, "请提供图片地址")
	}
	target, ok := normalizeMaskTarget(in.GetTarget())
	if !ok {
		return nil, xerr.Param("区域只支持 garment / product / person / background")
	}
	if _, err := normalizeMaskParts(target, in.GetParts()); err != nil {
		return nil, err
	}
	if owns == nil || !owns(u) {
		logx.WithContext(ctx).Infow("studio mask: image url not from own storage (check=mask_url_owned)",
			logx.Field("user_id", in.GetUserId()))
		return nil, xerr.Param("图片地址不是本服务的地址")
	}
	res, err := m.Get(ctx, in.GetUserId(), u, target, in.GetParts())
	if err != nil {
		return nil, err
	}
	return &studio.SegmentMaskResp{MaskUrl: res.URL, Target: res.Target, Cached: res.Cached,
		Parts: toMaskParts(res.Parts), AvailableParts: availableMaskParts(res.Target)}, nil
}

// toMaskParts 逐件蒙版 → proto。
func toMaskParts(parts []maskPart) []*studio.MaskPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]*studio.MaskPart, 0, len(parts))
	for _, p := range parts {
		out = append(out, &studio.MaskPart{Part: p.Part, MaskUrl: p.URL})
	}
	return out
}
