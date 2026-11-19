package logic

// 生图预估点数（只读）：契约 §AE。与 RunNode 冻结走同一条链路 —— 同样的 specModel 归一（档位 / 画质）、
// 同一个 c.charge 构造的 Charge、同一个 Billing.Quote（含 applyFreezeCap），所以报出来的就是冻结的数。
// 不冻结、不写库、不查余额；web 的「预计最多 X 点」调它，失败再退回静态目录价。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"
)

const (
	estimateLimitPerMin = 60 // 每用户每分钟最多查询次数（轻量保护；多副本各算各的）
	estimateMaxRefs     = 64
)

type estimateLimiter struct {
	mu   sync.Mutex
	hits map[int64][]time.Time
}

var estLimiter = &estimateLimiter{hits: map[int64][]time.Time{}}

// allow 滑动窗口 1 分钟计数；顺手清理过期用户，避免 map 无限增长。
func (l *estimateLimiter) allow(uid int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-time.Minute)
	keep := l.hits[uid][:0]
	for _, t := range l.hits[uid] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= estimateLimitPerMin {
		l.hits[uid] = keep
		return false
	}
	l.hits[uid] = append(keep, now)
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// Estimate 见文件注释。只支持图片模型（视频价目按时长，走目录静态价）。
func (c *core) Estimate(ctx context.Context, in *canvas.EstimateReq) (*canvas.EstimateResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	if !estLimiter.allow(in.UserId, time.Now()) {
		return &canvas.EstimateResp{Available: false, Reason: "查询太频繁，请稍后再试", Currency: "points"}, nil
	}
	c.refreshGroups(ctx)
	sp, err := c.estimateSpec(in)
	if err != nil {
		return nil, err
	}
	q := &quoteOut{Warnings: []string{}}
	q.setBackend(model.BackendGateway)
	c.quoteGateway(ctx, in.UserId, sp, q)
	out := &canvas.EstimateResp{
		Available: true, Currency: "points", ModelGroup: sp.ModelGroup, Size: sp.Size, Quality: sp.Quality,
		Count: int32(sp.Count), RefImages: int32(len(sp.Images)), BillingMode: q.BillingMode, Note: q.Note,
	}
	if q.Available != nil && !*q.Available {
		out.Available, out.Reason = false, q.Reason
		return out, nil
	}
	if !q.PriceKnown {
		out.Available, out.Reason = false, firstNonEmpty(q.Note, "暂时无法报价")
		return out, nil
	}
	out.Points, out.CapPoints, out.UnitPoints = q.Points, q.MaxPoints, q.Basis.UnitPoints
	out.SettleMode = strings.ToLower(firstNonEmpty(q.SettleMode, billing.SettleFixed))
	return out, nil
}

// estimateSpec 构造与受理一致的 runSpec（只含计价用到的字段）。
func (c *core) estimateSpec(in *canvas.EstimateReq) (*runSpec, error) {
	m, ok := c.s.Config.FindModel(in.Model, in.ModelId, "image")
	if !ok && strings.TrimSpace(in.ModelGroup) != "" {
		m, ok = c.s.Config.FindModel(in.ModelGroup, "", "image")
	}
	if !ok {
		return nil, xerr.Param(fmt.Sprintf("不支持的模型：%s", firstNonEmpty(in.ModelId, in.Model, in.ModelGroup)))
	}
	sp := &runSpec{Kind: model.KindImage, Backend: model.BackendGateway, Model: m, ModelGroup: m.ModelGroup,
		EntryCode: c.s.Config.EntryCodes.Image}
	sp.Size = strings.TrimSpace(in.Size)
	if sp.Size == "" && len(m.Sizes) > 0 {
		sp.Size = m.Sizes[0]
	}
	sp.Size, sp.Warnings = normalizeImageSize(sp.ModelGroup, sp.Size)
	if sp.Size != "" && len(m.Sizes) > 0 {
		v, ok := inList(sp.Size, m.Sizes)
		if !ok {
			return nil, xerr.Param(fmt.Sprintf("%s 不支持分辨率 %s", modelDisplay(m), sp.Size))
		}
		sp.Size = v
	}
	sp.Count = int(in.Count)
	if sp.Count <= 0 {
		sp.Count = 1
	}
	limit := m.CountMax
	if limit <= 0 {
		limit = 4
	}
	if sp.Count > limit {
		return nil, xerr.Param(fmt.Sprintf("count 取值 1–%d", limit))
	}
	// 画质与受理同口径：不支持画质的模型清空；支持的缺省 high（imageQuality 再兜一次）
	if len(m.Qualities) > 0 {
		sp.Quality = qualityOf(in.Quality)
	}
	sp.Quality = imageQuality(sp)
	refs := max(int(in.RefImages), len(in.RefImageSizes), 0)
	if lim := c.catalogImageRefs(m); lim > 0 && refs > lim {
		return nil, xerr.Param(fmt.Sprintf("%s 参考图最多 %d 张", modelDisplay(m), lim))
	}
	refs = min(refs, estimateMaxRefs)
	for i := 0; i < refs; i++ {
		im := refMedia{}
		if i < len(in.RefImageSizes) {
			if z := in.RefImageSizes[i]; z != nil && z.W > 0 && z.H > 0 && z.W <= 65535 && z.H <= 65535 {
				im.W, im.H = int(z.W), int(z.H)
			}
		}
		sp.Images = append(sp.Images, im)
	}
	// 提示词只用其长度估 token：占位串（charge 取 rune 数）
	if n := int(in.PromptChars); n > 0 {
		sp.Prompt = strings.Repeat("x", min(n, 100000))
	}
	return sp, nil
}
