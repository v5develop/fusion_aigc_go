package logic

import (
	"context"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/modelgroup"

	"github.com/zeromicro/go-zero/core/logx"
)

// groupCache gateway /v1/catalog/model-groups 的进程内缓存（60 秒）。连线校验与 catalog 共用。
type groupCache struct {
	mu sync.RWMutex
	at time.Time
	m  map[string]aigateway.ModelGroup
}

var gatewayGroups = &groupCache{}

const groupTTL = 60 * time.Second

// get 按组码取目录项；组码按 pkg/modelgroup 等价表展开（规范组 / 旧 Dev 组），网关目录里只有其中之一也能命中，
// 优先启用的、再优先规范组——画布与网关没有上线先后约束。
func (g *groupCache) get(code string) (aigateway.ModelGroup, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var first *aigateway.ModelGroup
	for _, c := range modelgroup.Aliases(code) {
		if x, ok := g.m[c]; ok {
			if x.Enabled {
				return x, true
			}
			if first == nil {
				first = &x
			}
		}
	}
	if first != nil {
		return *first, true
	}
	return aigateway.ModelGroup{}, false
}

func (g *groupCache) loaded() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.m != nil
}

func (g *groupCache) set(list []aigateway.ModelGroup) {
	m := make(map[string]aigateway.ModelGroup, len(list))
	for _, x := range list {
		m[x.Code] = x
	}
	g.mu.Lock()
	g.m, g.at = m, time.Now()
	g.mu.Unlock()
}

func (g *groupCache) stale() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return time.Since(g.at) > groupTTL
}

// refreshGroups 过期则拉一次 gateway 模型组（失败保留旧值，只记日志）。
func (c *core) refreshGroups(ctx context.Context) {
	if !c.s.Gateway.Enabled() || !gatewayGroups.stale() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := c.s.Gateway.ModelGroups(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 拉 gateway 模型组失败（沿用缓存 / 配置）: %v", err)
		gatewayGroups.mu.Lock()
		gatewayGroups.at = time.Now() // 失败也退避一个周期，别每个请求都打
		gatewayGroups.mu.Unlock()
		return
	}
	gatewayGroups.set(list)
}

// modelReady 模型组是否可用：gateway 未接入 → false；拉到了目录但没有该组或已停用 → false；没拉到目录 → 按可用。
func (c *core) modelReady(group string) bool {
	if !c.s.Gateway.Enabled() {
		return false
	}
	if !gatewayGroups.loaded() {
		return true
	}
	g, ok := gatewayGroups.get(group)
	return ok && g.Enabled
}

func (c *core) catalogModel(m config.ModelSpec) *canvas.CatalogModel {
	out := &canvas.CatalogModel{
		Model: m.Model, Label: m.DisplayName(), ModelId: m.ModelID, Kind: m.Kind, Badge: m.Badge, ModelGroup: m.ModelGroup,
		MaxImageRefs: int32(c.catalogImageRefs(m)), Ratios: m.Ratios, Sizes: m.Sizes, Qualities: m.Qualities,
		Durations: m.Durations, Sound: m.Sound, CountMax: int32(m.CountMax),
		CapQuality: len(m.Qualities) > 0, CapCount: m.Kind == "image" && m.CountMax > 1,
		CapDuration: m.Kind == "video", CapSound: m.Kind == "video" && m.Sound,
		Ready: c.modelReady(m.ModelGroup), MaxReferenceVideos: int32(m.MaxReferenceVideos),
	}
	if g, ok := gatewayGroups.get(m.ModelGroup); ok {
		fl := g.FeatureLimits
		if fl.MaxN != nil && m.Kind == "image" && int32(*fl.MaxN) < out.CountMax {
			out.CountMax = int32(*fl.MaxN)
		}
		if fl.MaxReferenceVideos != nil {
			out.MaxReferenceVideos = int32(*fl.MaxReferenceVideos)
		}
		if len(fl.AspectRatios) > 0 {
			out.Ratios = intersect(m.Ratios, fl.AspectRatios)
		}
	}
	for _, p := range m.Prices {
		out.Prices = append(out.Prices, &canvas.CatalogPrice{Size: p.Size, Duration: p.Duration, Points: p.Points})
	}
	return out
}

// intersect a 里在 b 中的（保持 a 的顺序）；交集为空时返回 a（gateway 没声明的维度不收窄）。
func intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[strings.ToLower(x)] = true
	}
	var out []string
	for _, x := range a {
		if set[strings.ToLower(x)] {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return a
	}
	return out
}

func (c *core) Catalog(ctx context.Context, in *canvas.CatalogReq) (*canvas.CatalogResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	bearer := c.rememberBearer(ctx, in.UserId)
	c.refreshGroups(ctx)
	out := &canvas.CatalogResp{}
	for _, m := range c.s.Config.ModelTable() {
		if m.Kind == "video" {
			out.VideoModels = append(out.VideoModels, c.catalogModel(m))
		} else {
			out.ImageModels = append(out.ImageModels, c.catalogModel(m))
		}
	}
	gw := c.s.Gateway.Enabled()
	out.Tools = []*canvas.CatalogTool{
		{Tool: "crop", Label: "裁剪", Billing: "free", Ready: c.s.Media != nil},
		{Tool: "upscale", Label: "高清放大", Billing: "java", Scales: []int32{2, 4}, Ready: c.s.Java.Enabled()},
		{Tool: "outpaint", Label: "扩图", Billing: "java", Ratios: append([]string(nil), outpaintRatios...), Ready: c.s.Java.Enabled()},
		{Tool: "mask", Label: "局部重绘", Billing: "java", Ready: c.s.Java.Enabled()},
		{Tool: "i2p", Label: "反推提示词", Billing: "gateway", Ready: gw && c.modelReady(c.s.Config.I2P.ModelGroup)},
		{Tool: "tryon", Label: "模特试穿", Billing: "studio", Ready: c.s.Studio != nil},
	}
	// Java 按用户报价的价目原样透传（拿不到不影响目录）
	if bearer != "" && c.s.Java.Enabled() {
		jctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if b, err := c.s.Java.ModelConfigs(jctx, bearer, false); err == nil {
			out.JavaImageConfigs = string(b)
		} else {
			logx.WithContext(ctx).Infof("canvas: 取 Java 图片价目失败: %v", err)
		}
		if b, err := c.s.Java.ModelConfigs(jctx, bearer, true); err == nil {
			out.JavaVideoConfigs = string(b)
		}
	}
	return out, nil
}

// outpaintRatios 扩图可选目标比例（Java ai-ratio-convert 的 aspectRatio）。
var outpaintRatios = []string{"1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2"}
