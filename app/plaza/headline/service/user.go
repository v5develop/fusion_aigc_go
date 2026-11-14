package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ErrIndustryRequired 用户端缺 industry（或只给了 subIndustry）：handler 据此回 HTTP 400。
var ErrIndustryRequired = xerr.New(xerr.ParamMissing, "请先选择行业（industry 必填）")

func errIndustryNotFound() error {
	return xerr.NewReason(xerr.NotFound, ReasonIndustryNotFound, "这个行业已调整，请重新选择")
}

// payloadHidden 下发 payload 时摘除的键（同 Java PlazaHeadlineService.PAYLOAD_HIDDEN_KEYS）。
var payloadHidden = []string{"agentSkillId", "agent_skill_id", "v1PlazaTemplateId", "prompt"}

// UserIndustry 条目所属的（启用）行业。
type UserIndustry struct {
	ID         int64  `json:"id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	ParentCode string `json:"parentCode,omitempty"`
}

// UserItem 用户端头条条目：沿用 v1 头条形状（headlineId…endDate），加 kind / usageVisible / 媒体与行业字段。
// source / score / score_detail 一律不下发。
type UserItem struct {
	HeadlineID      *int64          `json:"headlineId"`
	Rank            int             `json:"rank"`
	PlazaTemplateID int64           `json:"plazaTemplateId"`
	Title           string          `json:"title"`
	AgentSkillID    *string         `json:"agentSkillId"`
	Tags            []string        `json:"tags"`
	Payload         json.RawMessage `json:"payload"`
	Why             string          `json:"why"`
	Uses            int             `json:"uses"`
	PreviousUses    int             `json:"previousUses"`
	Trend           *int            `json:"trend"`
	IsNew           bool            `json:"isNew"`
	StartDate       *string         `json:"startDate"`
	EndDate         *string         `json:"endDate"`
	// Kind HEAT=按热度上榜（含人工）；NEW=冷启动补位（只显示「新上架」，不显示热度 / 趋势）
	Kind string `json:"kind"`
	// UsageVisible 近 7 天做同款达到展示门槛（默认 20 次）；false 时 uses / previousUses / trend 置空，只显示「热」标签和名次
	UsageVisible bool           `json:"usageVisible"`
	MediaType    string         `json:"mediaType"`
	VideoURL     *string        `json:"videoUrl"`
	PosterURL    *string        `json:"posterUrl"`
	Industries   []UserIndustry `json:"industries"`
}

// UserHeadlines GET /api/v1/plaza/headlines 的 data。
type UserHeadlines struct {
	Scope      string     `json:"scope"`
	Version    int64      `json:"version"`
	ComputedAt *string    `json:"computedAt"`
	StatsFrom  string     `json:"statsFrom"`
	StatsTo    string     `json:"statsTo"`
	Items      []UserItem `json:"items"`
}

// ResolveScope 用户端行业参数 → 范围：缺 industry（或只有 subIndustry）→ ErrIndustryRequired（HTTP 400）；
// industry 不存在 / 停用 / 是保留值 ALL、subIndustry 不属于它 → INDUSTRY_NOT_FOUND。
// 给了 subIndustry → 该二级（独立计算的榜）；只给 industry → 该行业（一级含其二级）。用户侧没有全局榜。
func ResolveScope(cat *store.Catalog, industry, sub string) (string, error) {
	industry, sub = strings.TrimSpace(industry), strings.TrimSpace(sub)
	if industry == "" {
		return "", ErrIndustryRequired
	}
	if industry == algo.ScopeAll || industry == algo.ScopeAny || cat == nil || !cat.IndustryReady {
		return "", errIndustryNotFound()
	}
	if _, ok := cat.Scope(industry); !ok {
		return "", errIndustryNotFound()
	}
	if sub == "" {
		return industry, nil
	}
	s, ok := cat.Scope(sub)
	if !ok || s.ParentCode != industry {
		return "", errIndustryNotFound()
	}
	return sub, nil
}

// Headlines 用户端按行业读头条：Redis 最新榜单（缺失读库并回填）→ 请求时再校验（下架 / 删除 / 已不属于该范围 /
// 已被抑制的丢弃，不重排）→ keyword 在该范围榜单内过滤（名次保持）。该范围在最新版本里不存在 → 空榜 + 异步触发刷新。
func (s *Service) Headlines(ctx context.Context, industry, sub, keyword string) (*UserHeadlines, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := ResolveScope(cat, industry, sub)
	if err != nil {
		return nil, err
	}
	now := s.now()
	payload, err := s.latestFor(ctx, scope)
	if err != nil {
		return nil, err
	}
	cfg, _, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	p := cfg.For(scope)
	out := &UserHeadlines{Scope: scope, Items: []UserItem{}}
	if payload == nil {
		out.StatsFrom, out.StatsTo = statsWindow(now)
		s.TriggerAsync(ctx, scope)
		return out, nil
	}
	out.Version = payload.Version
	ca := payload.ComputedAt.In(algo.CST).Format("2006-01-02T15:04:05")
	out.ComputedAt = &ca
	out.StatsFrom, out.StatsTo = statsWindow(payload.ComputedAt)
	if len(payload.Items) == 0 && payload.Version == 0 {
		s.TriggerAsync(ctx, scope)
	}

	rules, blocks, err := s.rulesAndBlocks(ctx, false)
	if err != nil {
		return nil, err
	}
	var ab []algo.Block
	for _, b := range blocks {
		ab = append(ab, b.Algo())
	}
	suppressed := algo.SuppressedSet(scope, ab, now)
	ruleByID := map[int64]store.HeadlineRule{}
	for _, r := range rules {
		ruleByID[r.ID] = r
	}
	sc, _ := cat.Scope(scope)
	needle := strings.ToLower(strings.TrimSpace(keyword))
	for _, it := range payload.Items {
		t := cat.Templates[it.TemplateID]
		if !t.VisibleAt(now) || !sc.HasMember(it.TemplateID) || suppressed[it.TemplateID] {
			continue // 下架 / 删除 / 改了行业 / 被抑制：不等下一轮刷新就从该榜消失
		}
		ui := s.userItem(cat, t, it, p, ruleByID)
		if needle != "" && !strings.Contains(searchText(t, ui), needle) {
			continue
		}
		out.Items = append(out.Items, ui)
	}
	return out, nil
}

// latestFor 某范围最新榜单：Redis → 库（并回填 Redis）。最新版本里没有该范围返回 nil。
func (s *Service) latestFor(ctx context.Context, scope string) (*latestPayload, error) {
	if b, err := s.sig.GetLatest(ctx, scope); err == nil && len(b) > 0 {
		var p latestPayload
		if json.Unmarshal(b, &p) == nil {
			return &p, nil
		}
	}
	snap, err := s.repo.LatestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, nil
	}
	rows, err := s.repo.Items(ctx, snap.ID, scope)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && !scopeComputed(snap.ScopeStats, scope) {
		return nil, nil
	}
	p := &latestPayload{Version: snap.ID, ComputedAt: snap.ComputedAt.Time, WindowStart: snap.WindowStart.Time, WindowEnd: snap.WindowEnd.Time, Scope: scope, Items: []algo.Item{}}
	for _, r := range rows {
		it := r.Algo()
		it.Detail, it.Score = nil, nil
		p.Items = append(p.Items, it)
	}
	if s.sig.Enabled() {
		if b, err := json.Marshal(p); err == nil {
			// 按版本号比较后写：慢请求读到的旧版不覆盖刷新刚写入的新版
			if n, err := s.sig.SetLatest(ctx, map[string][]byte{scope: b}, p.Version, s.conf.latestTTL(), nil); err != nil {
				logx.WithContext(ctx).Errorf("plaza headline: 回填榜单缓存失败: %v", err)
			} else if n == 0 {
				logx.WithContext(ctx).Infof("plaza headline: 跳过回填 scope=%s version=%d——判据：缓存里已有更新的版本", scope, p.Version)
			}
		}
	}
	return p, nil
}

// scopeComputed 该范围是否在这一版里计算过（空榜也算计算过，区别于「新行业还没算」）。
func scopeComputed(stats json.RawMessage, scope string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(stats, &m) != nil {
		return false
	}
	_, ok := m[scope]
	return ok
}

func statsWindow(at time.Time) (string, string) {
	to := at.In(algo.CST)
	return to.AddDate(0, 0, -6).Format("2006-01-02"), to.Format("2006-01-02")
}

func (s *Service) userItem(cat *store.Catalog, t *store.CatalogTemplate, it algo.Item, p algo.Params, rules map[int64]store.HeadlineRule) UserItem {
	ui := UserItem{Rank: it.Rank, PlazaTemplateID: it.TemplateID, Title: t.Title, Tags: t.Tags, Why: it.Why,
		Uses: it.Uses7d, PreviousUses: it.PrevUses, Trend: it.Trend, IsNew: it.IsNew, Kind: "HEAT", MediaType: t.MediaType}
	if ui.Tags == nil {
		ui.Tags = []string{}
	}
	if it.HeadlineID != 0 {
		id := it.HeadlineID
		ui.HeadlineID = &id
		if r, ok := rules[id]; ok {
			sd, ed := r.StartDate.In(algo.CST).Format("2006-01-02"), r.EndDate.In(algo.CST).Format("2006-01-02")
			ui.StartDate, ui.EndDate = &sd, &ed
		}
	}
	if it.Source == algo.SourceFill {
		ui.Kind, ui.Uses, ui.PreviousUses, ui.Trend, ui.IsNew = "NEW", 0, 0, nil, true
	} else {
		ui.UsageVisible = algo.UsageVisible(it.Uses7d, p)
		if !ui.UsageVisible {
			ui.Uses, ui.PreviousUses, ui.Trend = 0, 0, nil
		}
	}
	if t.VideoURL != "" {
		v := t.VideoURL
		ui.VideoURL = &v
	}
	if t.PosterURL != "" {
		v := t.PosterURL
		ui.PosterURL = &v
	}
	ui.Payload, ui.AgentSkillID = cleanPayload(t.Data)
	ui.Industries = []UserIndustry{}
	for _, id := range t.IndustryIDs {
		ind, ok := cat.IndustryByID(id)
		if !ok || !ind.Enabled {
			continue
		}
		u := UserIndustry{ID: ind.ID, Code: ind.Code, Name: ind.Name}
		if ind.ParentID != 0 {
			parent, ok := cat.IndustryByID(ind.ParentID)
			if !ok || !parent.Enabled {
				continue
			}
			u.ParentCode = parent.Code
		}
		ui.Industries = append(ui.Industries, u)
	}
	return ui
}

// cleanPayload 摘掉 prompt 与元数据键后的 payload（数字按原文保留，不经 float64 往返），以及字符串形式的 agentSkillId。
func cleanPayload(data json.RawMessage) (json.RawMessage, *string) {
	var m map[string]json.RawMessage
	if len(data) == 0 || json.Unmarshal(data, &m) != nil {
		return json.RawMessage("{}"), nil
	}
	var skill *string
	for _, k := range []string{"agentSkillId", "agent_skill_id"} {
		raw, ok := m[k]
		if !ok || skill != nil || string(raw) == "null" {
			continue
		}
		var str string
		if json.Unmarshal(raw, &str) == nil {
			skill = &str
		} else {
			v := string(bytes.TrimSpace(raw))
			skill = &v
		}
	}
	for _, k := range payloadHidden {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}"), skill
	}
	return b, skill
}

func searchText(t *store.CatalogTemplate, ui UserItem) string {
	var desc struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(t.Data, &desc)
	return strings.ToLower(strings.Join([]string{t.Title, t.Description, desc.Description, strings.Join(t.Tags, " "), ui.Why}, " "))
}
