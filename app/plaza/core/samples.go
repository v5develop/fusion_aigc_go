package core

import (
	"context"
	"strings"

	"aigc-local-life/app/plaza/model"

	"github.com/uptrace/bun"
)

// MaxSamples 用户侧一次最多返回的示例素材条数；后台每个范围也最多这么多条。
const MaxSamples = 6

// SampleItem 示例素材（用户侧）。
type SampleItem struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	ThumbURL string `json:"thumbUrl"`
	Title    string `json:"title"`
	Scope    string `json:"scope"` // TEMPLATE | INDUSTRY
	Role     string `json:"role"`  // INPUT（放进去的）| OUTPUT（做出来的）
}

// Samples GET /plaza/samples：先取该模板自己的示例（TEMPLATE），不足时补所选行业（及其一级）的示例（INDUSTRY），
// 绝不取其他行业的示例；最多 6 条。industry 必填；给了 templateId 则必须属于该行业且此刻可见。
func (s *Service) Samples(ctx context.Context, industry, sub string, templateID int64) ([]SampleItem, error) {
	if strings.TrimSpace(industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if s.db == nil {
		return nil, ErrNotReady()
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.resolve(c, industry, sub)
	if err != nil {
		return nil, err
	}
	if templateID < 0 {
		return nil, errParam("templateId 不合法")
	}
	if templateID > 0 {
		if _, ok := c.InScope(sc, templateID, s.now()); !ok {
			return nil, ErrTemplateGone()
		}
	}
	var rows []model.SampleAsset
	q := s.db.NewSelect().Model(&rows).Where("sa.deleted = 0").Where("sa.enabled = 1")
	indIDs := []int64{sc.Sel.ID}
	if sc.Sel.Parent != nil {
		indIDs = append(indIDs, sc.Sel.Parent.ID)
	}
	if templateID > 0 {
		q = q.WhereGroup(" AND ", func(g *bun.SelectQuery) *bun.SelectQuery {
			return g.Where("(sa.scope_type = ? AND sa.template_id = ?)", model.SampleScopeTemplate, templateID).
				WhereOr("(sa.scope_type = ? AND sa.industry_id IN (?))", model.SampleScopeIndustry, bun.In(indIDs))
		})
	} else {
		q = q.Where("sa.scope_type = ?", model.SampleScopeIndustry).Where("sa.industry_id IN (?)", bun.In(indIDs))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return pickSamples(rows, sc.Sel.ID), nil
}

// pickSamples 模板级在前；行业级所选行业自己在前、其一级在后；各自按 sort_order、id；最多 6 条。
func pickSamples(rows []model.SampleAsset, selID int64) []SampleItem {
	rank := func(r model.SampleAsset) int {
		switch {
		case r.ScopeType == model.SampleScopeTemplate:
			return 0
		case r.IndustryID.Int64 == selID:
			return 1
		default:
			return 2
		}
	}
	sorted := append([]model.SampleAsset(nil), rows...)
	sortSlice(sorted, func(a, b model.SampleAsset) bool {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID < b.ID
	})
	out := []SampleItem{}
	for _, r := range sorted {
		if len(out) >= MaxSamples {
			break
		}
		thumb := r.ThumbURL.String
		if thumb == "" {
			thumb = r.URL
		}
		out = append(out, SampleItem{ID: r.ID, Kind: r.Kind, URL: r.URL, ThumbURL: thumb, Title: r.Title, Scope: r.ScopeType, Role: roleOrDefault(r.Role)})
	}
	return out
}

// ---------------------------------------------------------------- 接着做 / 最近用过（16.5、17.6）

// RecentItem 「接着做」条目：模板基本字段 + 自己的使用情况。
type RecentItem struct {
	TemplateItem
	LastUsedAt       string   `json:"lastUsedAt"`
	UseCount         int      `json:"useCount"`
	Channels         []string `json:"channels"`
	SubIndustryCodes []string `json:"subIndustryCodes"`
}

// Recent GET /plaza/recent（需登录）：由 afv_agent_turn.plaza_template_id 按用户去重得出（只读）。
// 严格只含当前行业：再按「模板属于所选范围 + 此刻可见」过滤，没有任何跨行业的例外。
// 与 Tab 无关；mediaType 可选（image|video），头条 Tab 不传。limit 默认 12，上限 50。
func (s *Service) Recent(ctx context.Context, userID int64, industry, sub, mediaType string, limit int) ([]RecentItem, error) {
	if strings.TrimSpace(industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if mediaType != "" && mediaType != model.MediaImage && mediaType != model.MediaVideo {
		return nil, errParam("mediaType 只能是 image 或 video")
	}
	if s.rep == nil || s.db == nil {
		return nil, ErrNotReady()
	}
	if limit < 1 {
		limit = 12
	}
	if limit > 50 {
		limit = 50
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.resolve(c, industry, sub)
	if err != nil {
		return nil, err
	}
	uses, err := s.rep.RecentTurns(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	usage := s.usageMap(ctx)
	out := []RecentItem{}
	for _, u := range uses {
		rec, ok := c.InScope(sc, u.TemplateID, now)
		if !ok || (mediaType != "" && rec.t.MediaType != mediaType) {
			continue
		}
		it := RecentItem{TemplateItem: s.item(c, rec, usage, now), UseCount: u.UseCount,
			LastUsedAt: u.LastUsedAt.Format(timeLayout), Channels: []string{}, SubIndustryCodes: []string{}}
		for _, ch := range it.TargetChannels {
			it.Channels = append(it.Channels, ch.Code)
		}
		for _, ref := range it.Industries {
			if ref.ParentCode != "" {
				it.SubIndustryCodes = append(it.SubIndustryCodes, ref.Code)
			}
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// roleOrDefault 空 = OUTPUT（老行 / 未带 role 的写入）。
func roleOrDefault(r string) string {
	if r == "" {
		return model.SampleRoleOutput
	}
	return r
}
