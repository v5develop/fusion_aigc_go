package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 灵感推荐（CONTRACT §S，工具 replicate_trending，只读、免费）。
//
// 口径：近 7 天「已确认」方案，按「改法组合」（被替换 / 按描述修改的维度集合 + 声音策略）聚合，按去重用户数排序，最多 5 条。
//   - k-匿名：某个组合的去重用户数 < Config.TrendingMinUsers（默认 20）不展示；输出只有维度键 / 改法 / 声音策略 / 中文标签，
//     不含参考图、备注、台词、叠字等任何用户内容，也不含具体用户数。
//   - 行业：参数优先，缺省取用户在广场设置的主行业，再缺省全站；该行业下一个组合都凑不够 k 时回落全站（视图的 industry 写实际用的范围，fallback=true）。
//   - 每小时预计算：结果按行业缓存在进程内（TrendingCache，跨运行参数重建共用），读请求只读缓存；过期由轮询器（TickTrending）刷新，
//     从没算过的行业第一次被问到时现算一次。多实例各算各的（只读聚合，没有一致性要求）。
// ---------------------------------------------------------------------------------------------

const (
	// trendingWindow 统计窗口。
	trendingWindow = 7 * 24 * time.Hour
	// trendingTTL 缓存有效期（预计算周期）。
	trendingTTL = time.Hour
	// trendingMaxItems 最多返回几条。
	trendingMaxItems = 5
	// trendingScanLimit 一次预计算最多扫多少张已确认方案（防极端数据量把进程拖垮；超出只统计最近的）。
	trendingScanLimit = 200000
	// trendingPeriod 视图里的统计周期标识。
	trendingPeriod = "7d"
)

// TrendingRow 一张近期已确认的方案（只带聚合要用的两列）。
type TrendingRow struct {
	UserID      int64
	Annotations json.RawMessage
}

// TrendingStore 灵感推荐的读（生产实现 app/replicate/repo；不装配时 replicate_trending 回 7003）。
type TrendingStore interface {
	// ConfirmedPlanChanges since 之后确认出片的方案（status=CONFIRMED 且任务创建于 since 之后），新的在前，最多 limit 张。
	ConfirmedPlanChanges(ctx context.Context, since time.Time, limit int) ([]TrendingRow, error)
	// UserIndustries 这些用户在广场设置的行业 code（一级 + 二级的父级都算，没设置的用户不在 map 里）。
	UserIndustries(ctx context.Context, userIDs []int64) (map[int64][]string, error)
	// UserIndustry 一个用户的主行业（一级 code；没设置 / 没有返回空串）。只读复用广场的行业偏好，不写。
	UserIndustry(ctx context.Context, userID int64) (string, error)
}

// WithTrending 装配灵感推荐。cache 为 nil 时服务自建一份（运行参数重建服务时缓存会丢，生产传同一份）。
func WithTrending(st TrendingStore, cache *TrendingCache) Option {
	return func(s *Service) {
		s.trendStore = st
		if cache == nil {
			cache = NewTrendingCache(trendingTTL)
		}
		s.trend = cache
	}
}

// TrendingDim 一个被改动的维度。
type TrendingDim struct {
	Key  string `json:"key"`
	Mode string `json:"mode"` // replace / describe
}

// TrendingItem 一条推荐：一种热门改法组合。
type TrendingItem struct {
	// Label 给用户看的中文名，如「换模特 + 保留原声」
	Label string `json:"label"`
	// Desc 一句话说明（不含任何用户内容与人数）
	Desc string `json:"desc"`
	// Dims 这个组合改了哪些项（key = 维度键，mode = replace / describe；多数用户用的改法）
	Dims []TrendingDim `json:"dims"`
	// Audio 声音策略（keep / mute / revoice）；默认的重新配乐（regenerate）不出现
	Audio string `json:"audio,omitempty"`
	// SampleID 有示例用的是同一组合时给它（replicate_try_sample 可直接试）
	SampleID int64 `json:"sampleId,omitempty"`
}

// TrendingView replicate_trending 的结果。
type TrendingView struct {
	// Industry 实际统计的行业 code；空 = 全站
	Industry string `json:"industry"`
	Period   string `json:"period"`
	// Fallback true = 指定 / 默认的行业下数据不足（k-匿名），这里给的是全站的结果。add-only。
	Fallback bool           `json:"fallback,omitempty"`
	Items    []TrendingItem `json:"items"`
}

// TrendingCache 灵感推荐的预计算缓存（按行业 code，"" = 全站）。
type TrendingCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*trendEntry
}

type trendEntry struct {
	items []TrendingItem
	at    time.Time
}

// NewTrendingCache 新建缓存；ttl<=0 取 1 小时。
func NewTrendingCache(ttl time.Duration) *TrendingCache {
	if ttl <= 0 {
		ttl = trendingTTL
	}
	return &TrendingCache{ttl: ttl, m: map[string]*trendEntry{}}
}

func (c *TrendingCache) get(industry string) (*trendEntry, bool) {
	e := c.m[industry]
	return e, e != nil
}

// 把用户在改法里的动作翻成「换 / 改」+ 项名；项名按电商口径（人物说「模特」）。
var trendingNoun = map[string]string{
	"model": "模特", "garment": "服装", "accessory": "配饰", "product": "商品", "motion": "动作", "expression": "表情",
	"line": "台词", "camera": "镜头", "light": "光线", "scene": "场景", "style": "风格",
}

var trendingAudioLabel = map[string]string{AudioKeep: "保留原声", AudioMute: "静音", AudioRevoice: "换声音"}

// comboOf 一份改法的组合键与被改动的维度（按 DimKeys 顺序）。没改任何项且声音是默认的返回 ok=false（不算一种改法）。
func comboOf(ann Annotations) (key string, dims []TrendingDim, audio string, ok bool) {
	for _, k := range DimKeys {
		e, has := ann.Dims[k]
		if !has {
			continue
		}
		if e.Mode == ModeReplace || e.Mode == ModeDescribe {
			dims = append(dims, TrendingDim{Key: k, Mode: e.Mode})
		}
	}
	audio = ann.Audio
	if audio == "" {
		audio = AudioRegenerate
	}
	if len(dims) == 0 && audio == AudioRegenerate {
		return "", nil, "", false
	}
	keys := make([]string, len(dims))
	for i, d := range dims {
		keys[i] = d.Key
	}
	return strings.Join(keys, ",") + "|" + audio, dims, audio, true
}

type trendGroup struct {
	key   string
	audio string
	users map[int64]struct{}
	plans int
	// modes[维度键][改法] 该组合里各维度被怎么改的计数（取多数作展示，平手取 replace）
	modes map[string]map[string]int
	dims  []string
}

// aggregateTrending 把方案行按改法组合聚合：keep(uid) 决定一行算不算（行业过滤）；用户数 < minUsers 的组合丢弃；
// 按用户数、方案数降序，同数按组合键升序（稳定），最多 limit 组。纯函数，不碰库。
func aggregateTrending(rows []TrendingRow, keep func(uid int64) bool, minUsers, limit int) []*trendGroup {
	groups := map[string]*trendGroup{}
	for _, r := range rows {
		if keep != nil && !keep(r.UserID) {
			continue
		}
		var ann Annotations
		if len(r.Annotations) == 0 || json.Unmarshal(r.Annotations, &ann) != nil {
			continue
		}
		key, dims, audio, ok := comboOf(ann)
		if !ok {
			continue
		}
		g := groups[key]
		if g == nil {
			g = &trendGroup{key: key, audio: audio, users: map[int64]struct{}{}, modes: map[string]map[string]int{}}
			for _, d := range dims {
				g.dims = append(g.dims, d.Key)
			}
			groups[key] = g
		}
		g.users[r.UserID] = struct{}{}
		g.plans++
		for _, d := range dims {
			if g.modes[d.Key] == nil {
				g.modes[d.Key] = map[string]int{}
			}
			g.modes[d.Key][d.Mode]++
		}
	}
	out := make([]*trendGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.users) >= minUsers { // k-匿名
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].users) != len(out[j].users) {
			return len(out[i].users) > len(out[j].users)
		}
		if out[i].plans != out[j].plans {
			return out[i].plans > out[j].plans
		}
		return out[i].key < out[j].key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// trendingLabel 「换模特 + 改场景 + 保留原声」。
func trendingLabel(dims []TrendingDim, audio string) string {
	var parts []string
	for _, d := range dims {
		noun := trendingNoun[d.Key]
		if noun == "" {
			noun = DimLabel[d.Key]
		}
		verb := "换"
		if d.Mode == ModeDescribe {
			verb = "改"
		}
		parts = append(parts, verb+noun)
	}
	if l := trendingAudioLabel[audio]; l != "" {
		parts = append(parts, l)
	}
	return strings.Join(parts, " + ")
}

// majorityMode 一个维度在组合里多数用户的改法；平手取 replace。
func majorityMode(m map[string]int) string {
	if m[ModeDescribe] > m[ModeReplace] {
		return ModeDescribe
	}
	return ModeReplace
}

// trendingItems 聚合结果 → 对外条目；samples = 组合键 → 示例号。
func trendingItems(groups []*trendGroup, samples map[string]int64) []TrendingItem {
	items := make([]TrendingItem, 0, len(groups))
	for i, g := range groups {
		dims := make([]TrendingDim, 0, len(g.dims))
		for _, k := range g.dims {
			dims = append(dims, TrendingDim{Key: k, Mode: majorityMode(g.modes[k])})
		}
		it := TrendingItem{Label: trendingLabel(dims, g.audio), Dims: dims, SampleID: samples[g.key]}
		if g.audio != AudioRegenerate {
			it.Audio = g.audio
		}
		if i == 0 {
			it.Desc = "近 7 天复刻最多的改法"
		} else {
			it.Desc = fmt.Sprintf("近 7 天热门改法第 %d 名", i+1)
		}
		items = append(items, it)
	}
	return items
}

// trendingSamples 可直接试的示例：启用且分析就绪，按组合键取排在最前的一个。
func (s *Service) trendingSamples(ctx context.Context, industry string) map[string]int64 {
	out := map[string]int64{}
	if s.lib == nil {
		return out
	}
	list, err := s.lib.ListVisibleSamples(ctx, industry, maxSamples)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 灵感推荐读示例失败（不带示例号继续）: %v", err)
		return out
	}
	for _, smp := range list {
		if smp == nil || smp.AnalysisID == nil || len(smp.Annotations) == 0 {
			continue
		}
		var ann Annotations
		if json.Unmarshal(smp.Annotations, &ann) != nil {
			continue
		}
		if key, _, _, ok := comboOf(ann); ok {
			if _, dup := out[key]; !dup {
				out[key] = smp.ID
			}
		}
	}
	return out
}

// computeTrending 一次预计算：扫近 7 天已确认方案 → 按行业过滤 → 聚合。
func (s *Service) computeTrending(ctx context.Context, industry string) ([]TrendingItem, error) {
	rows, err := s.trendStore.ConfirmedPlanChanges(ctx, s.now().Add(-trendingWindow), trendingScanLimit)
	if err != nil {
		return nil, err
	}
	var keep func(uid int64) bool
	if industry != "" {
		seen := map[int64]bool{}
		var ids []int64
		for _, r := range rows {
			if !seen[r.UserID] {
				seen[r.UserID] = true
				ids = append(ids, r.UserID)
			}
		}
		inds, err := s.trendStore.UserIndustries(ctx, ids)
		if err != nil {
			return nil, err
		}
		keep = func(uid int64) bool {
			for _, c := range inds[uid] {
				if c == industry {
					return true
				}
			}
			return false
		}
	}
	groups := aggregateTrending(rows, keep, s.cfg.TrendingMinUsers, trendingMaxItems)
	return trendingItems(groups, s.trendingSamples(ctx, industry)), nil
}

// trendingFor 取一个行业的推荐：缓存新鲜直接用；过期 / 没有现算并写回；现算失败有旧值用旧值。
func (s *Service) trendingFor(ctx context.Context, industry string) ([]TrendingItem, error) {
	c := s.trend
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.get(industry)
	if ok && s.now().Sub(e.at) < c.ttl {
		return e.items, nil
	}
	items, err := s.computeTrending(ctx, industry)
	if err != nil {
		if ok {
			logx.WithContext(ctx).Errorf("replicate: 灵感推荐刷新失败 判据=trending_refresh_failed industry=%q（沿用旧结果）: %v", industry, err)
			return e.items, nil
		}
		return nil, err
	}
	c.m[industry] = &trendEntry{items: items, at: s.now()}
	return items, nil
}

// Trending replicate_trending：本周同行业复刻最多的改法。industry 空 = 取用户的行业偏好，再缺省全站。只读、免费。
func (s *Service) Trending(ctx context.Context, userID int64, industry string) (*TrendingView, error) {
	if s.trendStore == nil || s.trend == nil {
		return nil, xerr.New(xerr.NotReady, "灵感推荐暂未开放")
	}
	industry = strings.TrimSpace(industry)
	if len(industry) > 64 {
		return nil, xerr.Param("行业 code 太长了")
	}
	if industry == "" && userID > 0 {
		if c, err := s.trendStore.UserIndustry(ctx, userID); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 读用户行业偏好失败（按全站推荐）user=%d: %v", userID, err)
		} else {
			industry = c
		}
	}
	items, err := s.trendingFor(ctx, industry)
	if err != nil {
		return nil, err
	}
	v := &TrendingView{Industry: industry, Period: trendingPeriod, Items: items}
	if len(items) == 0 && industry != "" { // 该行业的人还不够多：回落全站
		if items, err = s.trendingFor(ctx, ""); err != nil {
			return nil, err
		}
		v.Industry, v.Items, v.Fallback = "", items, true
	}
	if v.Items == nil {
		v.Items = []TrendingItem{}
	}
	return v, nil
}

// TickTrending 轮询器每轮调一次：把全站与所有已缓存行业里过期的结果刷新（每小时实际只算一次）。从没算过的全站结果也在这里预热。
func (s *Service) TickTrending(ctx context.Context) {
	if s.trendStore == nil || s.trend == nil {
		return
	}
	s.trend.mu.Lock()
	due := []string{}
	if _, ok := s.trend.get(""); !ok {
		due = append(due, "")
	}
	for ind, e := range s.trend.m {
		if s.now().Sub(e.at) >= s.trend.ttl {
			due = append(due, ind)
		}
	}
	s.trend.mu.Unlock()
	for _, ind := range due {
		if _, err := s.trendingFor(ctx, ind); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 灵感推荐预计算失败 判据=trending_precompute_failed industry=%q: %v", ind, err)
		}
	}
}
