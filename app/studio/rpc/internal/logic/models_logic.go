package logic

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/modelgroup"
	"aigc-local-life/pkg/modellabel"
	"aigc-local-life/pkg/xerr"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"

	"github.com/zeromicro/go-zero/core/logx"
)

type ModelsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewModelsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ModelsLogic {
	return &ModelsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Models 模型档能力：组规则 ∩ gateway 目录 feature_limits；with_price 时按 Quote 同口径附单张点数。
func (l *ModelsLogic) Models(in *studio.ModelsReq) (*studio.ModelsResp, error) {
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = string(model.KindImageGen)
	}
	k := model.Kind(kind)
	if !k.Valid() {
		return nil, xerr.Param("kind 只能是 tryon / pose / imagegen")
	}
	c := l.svcCtx.Config
	groups := c.ModelGroupMap()
	items := modelCapabilities(kind, groups, studioCatalog.get(l.ctx, l.svcCtx.Gateway))
	out := &studio.ModelsResp{Kind: kind, Items: items}
	if in.WithPrice {
		var q quoter
		if l.svcCtx.Billing != nil {
			q = l.svcCtx.Billing
		}
		out.BillingMode = billing.ModeOff
		if q != nil {
			out.BillingMode = q.Mode()
		}
		entry := svc.EntryCode(c, k)
		for _, it := range items {
			it.Prices = make([]*studio.ModelResolutionPrice, 0, len(it.Resolutions))
			for _, res := range it.Resolutions {
				r := quoteStudio(l.ctx, q, entry, k, &studio.QuoteReq{UserId: in.UserId, Kind: kind, Model: it.Code, Resolution: res,
					AspectRatio: firstRatio(it.AspectRatios)}, groups)
				it.Prices = append(it.Prices, &studio.ModelResolutionPrice{Resolution: res, Available: r.Available, UnitPoints: r.UnitPoints})
			}
		}
	}
	return out, nil
}

func firstRatio(rs []string) string {
	for _, r := range rs {
		if r == "1:1" {
			return r
		}
	}
	if len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// modelOrder 前端展示顺序；配置里多出来的档按名字排在后面。
var modelOrder = []string{pipeline.ModelGeneral, pipeline.ModelGPT, pipeline.ModelGPT25, pipeline.ModelBanana, pipeline.ModelSeedream}

// groupLabels 见 pkg/modellabel（与 canvas 共用）；没登记的组回落到档位码。

var allResolutions = []string{"1k", "2k", "4k"}

// modelCapabilities 纯函数：每个模型档的可选项。catalog 为 nil = 目录拉不到（只按组规则）。
//
// 规则（与提交时的归一同源，见 pipeline.NormalizeForGroup / AspectForGroup / GPT26MaxRefs）：
//   - 分辨率 1k / 2k / 4k；旧 Seedream（5 Pro）不出 4K（提交会降 2K）；
//   - 比例为 pipeline.StandardAspectRatios；Seedream 5.5 去掉 4:5 / 5:4（提交会改 3:4 / 4:3）；
//   - 目录 feature_limits.resolutions / aspect_ratios 非空时再求交（交集为空视为口径不一致，忽略目录）；
//   - 参考图上限：目录 max_reference_images > 兜底表；GPT 2.6 再按 kind 收紧；imagegen 不超过 MaxImageGenRefs。
func modelCapabilities(kind string, groups map[string]string, catalog map[string]aigateway.ModelGroup) []*studio.ModelCapability {
	codes := make([]string, 0, len(groups))
	seen := map[string]bool{}
	for _, c := range modelOrder {
		if groups[c] != "" {
			codes = append(codes, c)
			seen[c] = true
		}
	}
	var extra []string
	for c, g := range groups {
		if !seen[c] && g != "" {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	codes = append(codes, extra...)

	out := make([]*studio.ModelCapability, 0, len(codes))
	for _, code := range codes {
		group := groups[code]
		it := &studio.ModelCapability{Code: code, ModelGroup: group, Available: true, AutoAspect: kind != pipeline.KindImageGen,
			Notes: []string{}, Prices: []*studio.ModelResolutionPrice{}}
		var cat *aigateway.ModelGroup
		if catalog != nil {
			if g, ok := catalogGroup(catalog, group); ok {
				cat = &g
			} else {
				it.Available = false
				it.Notes = append(it.Notes, "该模型暂不可用")
			}
		}
		// 展示名只用本仓维护的 groupLabels；没登记的组回落到档位码（通用 / GPT / …），不用 gateway 目录的 DisplayName（可能带供应商措辞）
		it.Label = code
		if l, ok := modellabel.Registered(group); ok {
			it.Label = l
		}
		if code == pipeline.ModelGeneral {
			it.Label = pipeline.ModelGeneral
		}

		res := append([]string(nil), allResolutions...)
		if pipeline.IsSeedreamGroup(group) && !pipeline.IsSeedream55Group(group) {
			res = []string{"1k", "2k"}
			it.Notes = append(it.Notes, "不支持 4K；1K 只支持 1:1，其它比例按 2K 生成")
		}
		ratios := append([]string(nil), pipeline.StandardAspectRatios...)
		if pipeline.IsSeedream55Group(group) {
			ratios = without(ratios, "4:5", "5:4")
			it.Notes = append(it.Notes, "不支持 4:5 / 5:4")
		}
		maxRefs := pipeline.ClampRefCap(pipeline.MaxRefImages(group))
		if cat != nil {
			res = intersectFold(res, cat.FeatureLimits.Resolutions)
			ratios = intersectFold(ratios, cat.FeatureLimits.AspectRatios)
			if n := cat.FeatureLimits.MaxReferenceImages; n != nil && *n > 0 {
				maxRefs = pipeline.ClampRefCap(*n) // 任何模型都不超过 14 张（与商拍组图同口径）
			}
		}
		if lim := pipeline.GPT26MaxRefs(group, kind); lim > 0 && lim < maxRefs {
			maxRefs = lim
			it.Notes = append(it.Notes, fmt.Sprintf("参考图最多 %d 张", lim))
		}
		if kind == pipeline.KindImageGen && maxRefs > MaxImageGenRefs {
			maxRefs = MaxImageGenRefs
		}
		it.Resolutions, it.AspectRatios, it.MaxRefImages = res, ratios, int32(maxRefs)
		out = append(out, it)
	}
	return out
}

// catalogGroup 按组码查 gateway 目录：组码按 pkg/modelgroup 等价表展开（规范组优先），
// 网关目录里只有规范组或只有旧 Dev 组都能命中（与网关没有上线先后约束）。
func catalogGroup(catalog map[string]aigateway.ModelGroup, group string) (aigateway.ModelGroup, bool) {
	for _, c := range modelgroup.Aliases(group) {
		if g, ok := catalog[c]; ok {
			return g, true
		}
	}
	return aigateway.ModelGroup{}, false
}

// without base 去掉 drop 里的项（保序）。
func without(base []string, drop ...string) []string {
	out := base[:0:0]
	for _, b := range base {
		if !slices.Contains(drop, b) {
			out = append(out, b)
		}
	}
	return out
}

// intersectFold base ∩ limit（大小写不敏感，保 base 顺序）；limit 为空或交集为空时原样返回 base。
func intersectFold(base, limit []string) []string {
	if len(limit) == 0 {
		return base
	}
	var out []string
	for _, b := range base {
		for _, l := range limit {
			if strings.EqualFold(strings.TrimSpace(l), b) {
				out = append(out, b)
				break
			}
		}
	}
	if len(out) == 0 {
		return base
	}
	return out
}

// catalogRefCap 目录里某模型组的 feature_limits.max_reference_images；没有目录 / 没这个组 / 没标返回 0（调用方走兜底表）。
// 与 flow.catalogMaxRefs（生图组装用）同口径：都只认 enabled 的组、>0 才算。
func catalogRefCap(cat map[string]aigateway.ModelGroup, group string) int {
	if g, ok := cat[group]; ok {
		if n := g.FeatureLimits.MaxReferenceImages; n != nil && *n > 0 {
			return *n
		}
	}
	return 0
}

// catalogTTL 内的目录缓存（与 flow.catalogMaxRefs 同口径：过滤 enabled=false；拉取失败 1 分钟后再试）。
type catalogCache struct {
	mu    sync.Mutex
	at    time.Time
	data  map[string]aigateway.ModelGroup
	ok    bool
	retry time.Time
}

var studioCatalog = &catalogCache{}

// get 启用的模型组；gateway 没配或拉不到返回 nil（调用方只按组规则）。
func (c *catalogCache) get(ctx context.Context, gw *aigateway.Client) map[string]aigateway.ModelGroup {
	if gw == nil || !gw.Enabled() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.ok && now.Sub(c.at) < catalogTTL {
		return c.data
	}
	if now.Before(c.retry) {
		if c.ok {
			return c.data
		}
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	groups, err := gw.ModelGroups(cctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 模型档能力拉 gateway 目录失败，按组规则返回: %v", err)
		c.retry = now.Add(time.Minute)
		if c.ok {
			return c.data
		}
		return nil
	}
	m := make(map[string]aigateway.ModelGroup, len(groups))
	for _, g := range groups {
		if g.Enabled {
			m[g.Code] = g
		}
	}
	c.data, c.at, c.ok = m, now, true
	return m
}
