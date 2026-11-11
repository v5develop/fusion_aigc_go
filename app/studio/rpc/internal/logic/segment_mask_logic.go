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

// segmentMask 校验入参与地址来源后取蒙版。来源优先按请求带的标识精确取（source_rid 画布候选 / task_id 子任务，见 mask_source.go
// 文件头），否则地址只认本存储签发或白名单前缀（素材库域名），或该用户自己成品的私有签名地址（mr:// / mrw:// 读时现签：反查 rid +
// 归属校验，兜底口径）。私有成品交给网关的是按 rid 现签的干净地址，缓存按 mr://{rid}：不替任意第三方地址下载。
func segmentMask(ctx context.Context, m *masker, owns func(string) bool, in *studio.SegmentMaskReq) (*studio.SegmentMaskResp, error) {
	if in.GetUserId() <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	u := strings.TrimSpace(in.GetImageUrl())
	taskID, srcRID := strings.TrimSpace(in.GetTaskId()), strings.TrimSpace(in.GetSourceRid())
	if taskID != "" && srcRID != "" {
		return nil, xerr.Param("task_id 与画布候选只能给一个")
	}
	if u == "" && taskID == "" && srcRID == "" {
		return nil, xerr.New(xerr.ParamMissing, "请提供图片地址")
	}
	target, ok := normalizeMaskTarget(in.GetTarget())
	if !ok {
		return nil, xerr.Param("区域只支持 garment / product / person / background")
	}
	if _, err := normalizeMaskParts(target, in.GetParts()); err != nil {
		return nil, err
	}
	var (
		res maskResult
		err error
	)
	switch {
	case srcRID != "":
		// 画布候选：网关已经 canvas-rpc 确认归属，只差现签
		src, serr := m.signRID(ctx, srcRID)
		if serr != nil {
			return nil, serr
		}
		res, err = m.GetPrivate(ctx, in.GetUserId(), srcRID, src, target, in.GetParts())
	case taskID != "":
		rid, src, public, ok, terr := m.resolveTask(ctx, in.GetUserId(), taskID, u)
		if terr != nil {
			return nil, terr
		}
		if !ok {
			return nil, xerr.Param("任务不存在或还没有成品")
		}
		if rid != "" {
			res, err = m.GetPrivate(ctx, in.GetUserId(), rid, src, target, in.GetParts())
		} else {
			res, err = m.Get(ctx, in.GetUserId(), public, target, in.GetParts())
		}
	case owns != nil && owns(u):
		res, err = m.Get(ctx, in.GetUserId(), u, target, in.GetParts())
	default:
		rid, src, ok := m.resolveOwned(ctx, in.GetUserId(), u)
		if !ok {
			logx.WithContext(ctx).Infow("studio mask: image url not from own storage nor own private result (check=mask_url_owned)",
				logx.Field("user_id", in.GetUserId()))
			return nil, xerr.Param("图片地址不是本服务的地址")
		}
		res, err = m.GetPrivate(ctx, in.GetUserId(), rid, src, target, in.GetParts())
	}
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
