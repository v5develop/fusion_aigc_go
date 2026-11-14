package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
)

// 模板状态（Go_V1.0.33 的生命周期列）。
const StatePublished = "PUBLISHED"

// CatalogTemplate 头条需要的模板字段（只读投影；行业 / 模板的写入归行业模块）。
type CatalogTemplate struct {
	ID          int64
	Title       string
	Description string
	Tags        []string
	Data        json.RawMessage // data_json 原文
	MediaType   string
	VideoURL    string
	PosterURL   string
	State       string
	OnlineAt    *time.Time
	OfflineAt   *time.Time
	Enabled     bool
	Deleted     bool
	// RequireEnabled 写者仍是 Java（Plaza.AdminWriteEnabled=false）时为 true：Java 只维护 enabled，不维护 state，
	// 此时 state+时间窗口之外还要求 enabled=1，避免 Java 后台下线（enabled=0）后的模板仍按 state=PUBLISHED 对外可见。
	RequireEnabled bool
	CreatedAt      time.Time
	IndustryIDs    []int64
}

// Cover 封面：视频优先 poster_url，否则 data_json.result[0]。
func (t *CatalogTemplate) Cover() string {
	if t.MediaType == "video" && t.PosterURL != "" {
		return t.PosterURL
	}
	var d struct {
		Result []string `json:"result"`
	}
	if json.Unmarshal(t.Data, &d) == nil && len(d.Result) > 0 {
		return strings.TrimSpace(d.Result[0])
	}
	return t.PosterURL
}

// VisibleAt 对用户可见：未删除、已发布、当前时刻在 [online_at, offline_at) 内、有封面；
// RequireEnabled（Java 仍是写者）时另要求 enabled=1。Go 为写者时不依赖派生的 enabled（读时按窗口过滤）。
func (t *CatalogTemplate) VisibleAt(now time.Time) bool {
	if t == nil || t.Deleted {
		return false
	}
	if t.State != "" {
		if t.State != StatePublished {
			return false
		}
		if t.OnlineAt != nil && now.Before(*t.OnlineAt) {
			return false
		}
		if t.OfflineAt != nil && !now.Before(*t.OfflineAt) {
			return false
		}
		if t.RequireEnabled && !t.Enabled {
			return false
		}
	} else if !t.Enabled { // 生命周期列还没上线：沿用 Java 的 enabled
		return false
	}
	return t.Cover() != ""
}

// Catalog 一次装载的目录：行业树 + 模板 + 关系 + 推导出的范围。
type Catalog struct {
	Industries []algo.Industry
	Templates  map[int64]*CatalogTemplate
	Scopes     []algo.Scope
	LoadedAt   time.Time
	// IndustryReady 行业表是否存在（不存在时只有后台 ALL 榜）
	IndustryReady bool

	byCode   map[string]algo.Industry
	scopeIdx map[string]int
}

// NewCatalog 由原始数据构建目录（推导范围、建索引）。也供单测直接构造。
func NewCatalog(industries []algo.Industry, templates map[int64]*CatalogTemplate, loadedAt time.Time, industryReady bool) *Catalog {
	rel := map[int64][]int64{}
	for id, t := range templates {
		if len(t.IndustryIDs) > 0 {
			rel[id] = t.IndustryIDs
		}
	}
	c := &Catalog{Industries: industries, Templates: templates, LoadedAt: loadedAt, IndustryReady: industryReady,
		byCode: map[string]algo.Industry{}, scopeIdx: map[string]int{}}
	for _, ind := range industries {
		c.byCode[ind.Code] = ind
	}
	c.Scopes = algo.BuildScopes(industries, rel)
	for i, s := range c.Scopes {
		c.scopeIdx[s.Code] = i
	}
	return c
}

// Industry 按 code 取行业。
func (c *Catalog) Industry(code string) (algo.Industry, bool) {
	ind, ok := c.byCode[code]
	return ind, ok
}

// IndustryByID 按 id 取行业。
func (c *Catalog) IndustryByID(id int64) (algo.Industry, bool) {
	for _, ind := range c.Industries {
		if ind.ID == id {
			return ind, true
		}
	}
	return algo.Industry{}, false
}

// Scope 按 code 取可计算的范围（启用的行业）。
func (c *Catalog) Scope(code string) (algo.Scope, bool) {
	i, ok := c.scopeIdx[code]
	if !ok {
		return algo.Scope{}, false
	}
	return c.Scopes[i], true
}

// ScopeCodes 全部可计算范围的 code 集合。
func (c *Catalog) ScopeCodes() map[string]bool {
	out := make(map[string]bool, len(c.Scopes))
	for _, s := range c.Scopes {
		out[s.Code] = true
	}
	return out
}

// ScopesOf 包含该模板的全部行业范围。
func (c *Catalog) ScopesOf(tplID int64) []string { return algo.ScopesOfTemplate(c.Scopes, tplID) }

// AlgoTemplates 算法输入（可见性按 now 判定）。
func (c *Catalog) AlgoTemplates(now time.Time) map[int64]algo.Template {
	out := make(map[int64]algo.Template, len(c.Templates))
	for id, t := range c.Templates {
		out[id] = algo.Template{ID: id, MediaType: mediaOr(t.MediaType), CreatedAt: t.CreatedAt, Visible: t.VisibleAt(now)}
	}
	return out
}

// AllScopeMembers 后台 ALL 榜的成员：全部模板 id（可见性在算法里判）。
func (c *Catalog) AllScopeMembers() []int64 {
	out := make([]int64, 0, len(c.Templates))
	for id := range c.Templates {
		out = append(out, id)
	}
	return out
}

func mediaOr(m string) string {
	if m == "" {
		return "image"
	}
	return m
}

// ---------------------------------------------------------------- SQL 读取

type industryRow struct {
	ID       int64  `bun:"id"`
	Code     string `bun:"code"`
	Name     string `bun:"name"`
	ParentID *int64 `bun:"parent_id"`
	Enabled  int    `bun:"enabled"`
	Sort     int    `bun:"sort_order"`
}

type templateRow struct {
	ID          int64           `bun:"id"`
	Title       string          `bun:"title"`
	Description *string         `bun:"description"`
	TagsJSON    json.RawMessage `bun:"tags_json"`
	DataJSON    json.RawMessage `bun:"data_json"`
	Enabled     int             `bun:"enabled"`
	CreateTime  CSTTime         `bun:"create_time"`
	MediaType   *string         `bun:"media_type"`
	VideoURL    *string         `bun:"video_url"`
	PosterURL   *string         `bun:"poster_url"`
	State       *string         `bun:"state"`
	OnlineAt    CSTTime         `bun:"online_at"`
	OfflineAt   CSTTime         `bun:"offline_at"`
}

type relationRow struct {
	TemplateID int64 `bun:"template_id"`
	IndustryID int64 `bun:"industry_id"`
}

const (
	templateColsFull = "id, title, description, tags_json, data_json, enabled, create_time, media_type, video_url, poster_url, state, online_at, offline_at"
	// 生命周期列还没加（Go_V1.0.33 未执行）时的旧口径
	templateColsLegacy = "id, title, description, tags_json, data_json, enabled, create_time"
)

// LoadCatalog 读目录（只读）：行业树、未删除的模板、模板↔行业关系。
// 行业表不存在 → 无行业（只有后台 ALL 榜）；模板表缺生命周期列 → 按 Java 的 enabled 判可见。
func (r *Repo) LoadCatalog(ctx context.Context, now time.Time) (*Catalog, error) {
	var tpls []templateRow
	err := r.DB.NewRaw("SELECT "+templateColsFull+" FROM afv_compat_plaza_template WHERE deleted = 0").Scan(ctx, &tpls)
	if IsMissingColumn(err) {
		tpls = nil
		err = r.DB.NewRaw("SELECT "+templateColsLegacy+" FROM afv_compat_plaza_template WHERE deleted = 0").Scan(ctx, &tpls)
	}
	if err != nil {
		return nil, err
	}
	templates := make(map[int64]*CatalogTemplate, len(tpls))
	for _, t := range tpls {
		ct := &CatalogTemplate{ID: t.ID, Title: t.Title, Data: t.DataJSON, Enabled: t.Enabled == 1, RequireEnabled: r.RequireEnabled, CreatedAt: t.CreateTime.Time,
			MediaType: "image", OnlineAt: t.OnlineAt.Ptr(), OfflineAt: t.OfflineAt.Ptr()}
		if t.Description != nil {
			ct.Description = *t.Description
		}
		if t.MediaType != nil && *t.MediaType != "" {
			ct.MediaType = *t.MediaType
		}
		if t.VideoURL != nil {
			ct.VideoURL = *t.VideoURL
		}
		if t.PosterURL != nil {
			ct.PosterURL = *t.PosterURL
		}
		if t.State != nil {
			ct.State = *t.State
		}
		_ = json.Unmarshal(t.TagsJSON, &ct.Tags)
		templates[t.ID] = ct
	}

	var inds []industryRow
	err = r.DB.NewRaw("SELECT id, code, name, parent_id, enabled, sort_order FROM afv_template_industry WHERE deleted = 0").Scan(ctx, &inds)
	if IsMissingTable(err) {
		return NewCatalog(nil, templates, now, false), nil
	}
	if err != nil {
		return nil, err
	}
	var rels []relationRow
	if err := r.DB.NewRaw("SELECT template_id, industry_id FROM afv_plaza_template_industry").Scan(ctx, &rels); err != nil && !IsMissingTable(err) {
		return nil, err
	}
	for _, rel := range rels {
		if t, ok := templates[rel.TemplateID]; ok {
			t.IndustryIDs = append(t.IndustryIDs, rel.IndustryID)
		}
	}
	industries := make([]algo.Industry, 0, len(inds))
	for _, ind := range inds {
		a := algo.Industry{ID: ind.ID, Code: ind.Code, Name: ind.Name, Enabled: ind.Enabled == 1, Sort: ind.Sort}
		if ind.ParentID != nil {
			a.ParentID = *ind.ParentID
		}
		industries = append(industries, a)
	}
	return NewCatalog(industries, templates, now, true), nil
}
