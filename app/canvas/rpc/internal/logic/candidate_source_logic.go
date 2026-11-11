package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CandidateSourceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCandidateSourceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CandidateSourceLogic {
	return &CandidateSourceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 只增（2026-11-11）：成功 run 某个图片候选的来源（仅本人、未删除）：私有引用（mr:// / mrw://，转存素材库前 / 失败 / 免费档）给 rid，
// 否则给长期地址。供区域蒙版按归属精确取图。
func (l *CandidateSourceLogic) CandidateSource(in *canvas.CandidateSourceReq) (*canvas.CandidateSourceResp, error) {
	return newCore(l.svcCtx).CandidateSource(l.ctx, in)
}

// CandidateSource 见 CandidateSourceLogic。归属与 GET /runs/:id 同口径（ownedRun：本人、未删除）；只认成功的图片类 run
// （视频 / 反推没有可识别区域的图片候选）。rid / 地址只给内部调用方（网关），不下发给前端。
func (c *core) CandidateSource(ctx context.Context, in *canvas.CandidateSourceReq) (*canvas.CandidateSourceResp, error) {
	r, err := c.ownedRun(ctx, in.GetUserId(), in.GetId())
	if err != nil {
		return nil, err
	}
	if r.Status != model.RunSucceeded {
		return nil, xerr.New(xerr.Conflict, "这次执行还没有成功的图片")
	}
	if r.Kind == model.KindVideo || r.Kind == model.KindToolI2P {
		return nil, xerr.Param("这次执行的产物不是图片")
	}
	cands := r.CandidateList()
	idx := int(r.SelectedIndex)
	if in.CandidateIndex != nil {
		idx = int(in.GetCandidateIndex())
	}
	if idx < 0 || idx >= len(cands) {
		return nil, xerr.Param("候选下标不合法")
	}
	v := strings.TrimSpace(cands[idx].URL)
	if rid, _, ok := refRID(v); ok {
		if rid == "" {
			return nil, xerr.Param("这张图片暂时不可用")
		}
		return &canvas.CandidateSourceResp{Rid: rid, Index: int32(idx)}, nil
	}
	if v == "" {
		return nil, xerr.Param("这张图片暂时不可用")
	}
	return &canvas.CandidateSourceResp{Url: v, Index: int32(idx)}, nil
}
