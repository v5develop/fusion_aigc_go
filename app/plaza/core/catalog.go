package core

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/app/plaza/model"
)

// ReservedAll 保留范围：全局总榜只存在于后台，用户侧传它等同于行业不存在。
const ReservedAll = "ALL"

// Source 目录快照的数据来源（repo 实现；单测用内存假实现）。
type Source interface {
	// Industries 全部未删除的行业（含停用）。
	Industries(ctx context.Context) ([]model.Industry, error)
	// Relations 全部模板↔行业关系。
	Relations(ctx context.Context) ([]model.TemplateIndustry, error)
	// PublishedTemplates 未删除且 state=PUBLISHED 的模板（上下线窗口在内存里按请求时刻过滤，所以精确到秒、不依赖定时任务）。
	PublishedTemplates(ctx context.Context) ([]model.Template, error)
	// Channels 渠道字典（含停用）。
	Channels(ctx context.Context) ([]model.Channel, error)
}

// IndustryNode 行业树节点。
type IndustryNode struct {
	model.Industry
	Parent   *IndustryNode
	Children []*IndustryNode // 含停用的子级（停用子级的模板仍归父级）
}

// effective 自身启用且（若是二级）父级也有效：父级停用整棵不可见。
func (n *IndustryNode) effective() bool {
	if n == nil || n.Enabled != 1 || n.Deleted != 0 {
		return false
	}
	return n.Parent == nil || n.Parent.effective()
}

func (n *IndustryNode) parentCode() string {
	if n.Parent == nil {
		return ""
	}
	return n.Parent.Code
}

type tplRec struct {
	t    model.Template
	rels []model.TemplateIndustry
	// requireEnabled 用户端口径是否额外要求 enabled=1：写权属还在 Java（Plaza.AdminWriteEnabled=false）时，
	// Java 只维护 enabled、不维护 state / 上下线窗口，只看 state 会把 Java 已停用（enabled=0）的模板放出来。
	requireEnabled bool
}

// Catalog 一份只读的目录快照：行业树 + 已发布模板 + 关系索引 + 渠道字典。
// 所有可见性判断都以请求时刻 now 为准（读时过滤）。
type Catalog struct {
	LoadedAt time.Time

	byID       map[int64]*IndustryNode
	byCode     map[string]*IndustryNode
	roots      []*IndustryNode
	tpls       map[int64]*tplRec
	byIndustry map[int64][]*tplRec
	channels   map[string]model.Channel
}

// BuildCatalog 从数据源装载。requireEnabled 见 tplRec.requireEnabled（= !Plaza.AdminWriteEnabled）。
func BuildCatalog(ctx context.Context, src Source, now time.Time, requireEnabled bool) (*Catalog, error) {
	inds, err := src.Industries(ctx)
	if err != nil {
		return nil, err
	}
	rels, err := src.Relations(ctx)
	if err != nil {
		return nil, err
	}
	tpls, err := src.PublishedTemplates(ctx)
	if err != nil {
		return nil, err
	}
	chs, err := src.Channels(ctx)
	if err != nil {
		return nil, err
	}
	return newCatalog(inds, rels, tpls, chs, now, requireEnabled), nil
}

func newCatalog(inds []model.Industry, rels []model.TemplateIndustry, tpls []model.Template, chs []model.Channel, now time.Time, requireEnabled bool) *Catalog {
	c := &Catalog{
		LoadedAt:   now,
		byID:       map[int64]*IndustryNode{},
		byCode:     map[string]*IndustryNode{},
		tpls:       map[int64]*tplRec{},
		byIndustry: map[int64][]*tplRec{},
		channels:   map[string]model.Channel{},
	}
	for _, in := range inds {
		if in.Deleted != 0 {
			continue
		}
		n := &IndustryNode{Industry: in}
		c.byID[in.ID] = n
		c.byCode[in.Code] = n
	}
	for _, n := range c.byID {
		if n.ParentID.Valid {
			if p := c.byID[n.ParentID.Int64]; p != nil && !p.ParentID.Valid { // 只允许两级：父级必须是一级
				n.Parent = p
				p.Children = append(p.Children, n)
				continue
			}
			continue // 父级不存在 / 已删除 / 本身是二级：孤儿，不进树
		}
		c.roots = append(c.roots, n)
	}
	less := func(a, b *IndustryNode) bool {
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID < b.ID
	}
	sort.Slice(c.roots, func(i, j int) bool { return less(c.roots[i], c.roots[j]) })
	for _, r := range c.roots {
		ch := r.Children
		sort.Slice(ch, func(i, j int) bool { return less(ch[i], ch[j]) })
	}
	for _, t := range tpls {
		if t.Deleted != 0 {
			continue
		}
		c.tpls[t.ID] = &tplRec{t: t, requireEnabled: requireEnabled}
	}
	for _, r := range rels {
		rec := c.tpls[r.TemplateID]
		if rec == nil || c.byID[r.IndustryID] == nil {
			continue
		}
		rec.rels = append(rec.rels, r)
		c.byIndustry[r.IndustryID] = append(c.byIndustry[r.IndustryID], rec)
	}
	for _, ch := range chs {
		c.channels[ch.Code] = ch
	}
	return c
}

// visibleAt 模板此刻对用户可见：已发布（装载时已过滤）且在 [online_at, offline_at) 内；
// 写权属还在 Java 时（requireEnabled）同时要求 enabled=1。
func (r *tplRec) visibleAt(now time.Time) bool {
	t := &r.t
	if t.Deleted != 0 || t.State != model.StatePublished {
		return false
	}
	if r.requireEnabled && t.Enabled != 1 {
		return false
	}
	if t.OnlineAt.Valid && now.Before(t.OnlineAt.Time) {
		return false
	}
	if t.OfflineAt.Valid && !now.Before(t.OfflineAt.Time) {
		return false
	}
	return true
}

// Scope 一次用户请求解析出的范围：所选行业（一级含其全部二级，或单个二级）。
type Scope struct {
	Root *IndustryNode // 一级
	Sub  *IndustryNode // 所选二级（可空）
	Sel  *IndustryNode // 实际选中的节点：Sub 或 Root
	ids  []int64       // 覆盖的行业 id
}

// Code 范围代码（二级优先）。
func (s *Scope) Code() string { return s.Sel.Code }

// Resolve 把 (industry, subIndustry) 解析成范围。
//   - industry 为空 → ErrIndustryRequired（HTTP 400）；只给 sub 同样。
//   - 保留值 ALL、不存在、已停用（含父级停用）→ ErrIndustryNotFound。
//   - industry 允许是一级或二级 code；sub 必须是 industry（一级）的有效子级。
func (c *Catalog) Resolve(industry, sub string) (*Scope, error) {
	industry, sub = strings.TrimSpace(industry), strings.TrimSpace(sub)
	if industry == "" {
		return nil, ErrIndustryRequired()
	}
	if strings.EqualFold(industry, ReservedAll) {
		return nil, ErrIndustryNotFound()
	}
	n := c.byCode[industry]
	if n == nil || !n.effective() {
		return nil, ErrIndustryNotFound()
	}
	if sub == "" {
		return c.scopeOf(n), nil
	}
	if strings.EqualFold(sub, ReservedAll) {
		return nil, ErrIndustryNotFound()
	}
	s := c.byCode[sub]
	if s == nil || !s.effective() || n.Parent != nil || s.Parent != n {
		return nil, ErrIndustryNotFound()
	}
	return c.scopeOf(s), nil
}

func (c *Catalog) scopeOf(sel *IndustryNode) *Scope {
	sc := &Scope{Sel: sel}
	if sel.Parent != nil {
		sc.Root, sc.Sub = sel.Parent, sel
		sc.ids = []int64{sel.ID}
		return sc
	}
	sc.Root = sel
	sc.ids = []int64{sel.ID}
	for _, ch := range sel.Children { // 停用的子级其模板仍归父级
		sc.ids = append(sc.ids, ch.ID)
	}
	return sc
}

// members 范围内此刻可见的模板（去重），附带「在本范围关系行里的最小 sort_order」。已按行业内顺序排好：
// minSort 升序、update_time 降序、id 降序。
type member struct {
	rec     *tplRec
	minSort int
}

func (c *Catalog) members(sc *Scope, now time.Time) []member {
	best := map[int64]int{}
	var order []*tplRec
	for _, id := range sc.ids {
		for _, rec := range c.byIndustry[id] {
			if !rec.visibleAt(now) {
				continue
			}
			for _, r := range rec.rels {
				if r.IndustryID != id {
					continue
				}
				if cur, ok := best[rec.t.ID]; !ok {
					best[rec.t.ID] = r.SortOrder
					order = append(order, rec)
				} else if r.SortOrder < cur {
					best[rec.t.ID] = r.SortOrder
				}
			}
		}
	}
	out := make([]member, 0, len(order))
	for _, rec := range order {
		out = append(out, member{rec, best[rec.t.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.minSort != b.minSort {
			return a.minSort < b.minSort
		}
		if !a.rec.t.UpdateTime.Equal(b.rec.t.UpdateTime) {
			return a.rec.t.UpdateTime.After(b.rec.t.UpdateTime)
		}
		return a.rec.t.ID > b.rec.t.ID
	})
	return out
}

// ListQuery 列表参数（已校验）。
type ListQuery struct {
	MediaType string // "" | image | video
	Keyword   string
	Page      int
	Size      int
}

// ListResult 一页结果；Total 是过滤后的总数。
type ListResult struct {
	Recs  []*tplRec
	Total int
}

// List 范围内的列表与搜索：只在所选范围内按标题 / 描述 / 标签做不区分大小写的包含匹配，不存在跨行业搜索。
func (c *Catalog) List(sc *Scope, q ListQuery, now time.Time) ListResult {
	kw := normKeyword(q.Keyword)
	var hit []*tplRec
	for _, m := range c.members(sc, now) {
		if q.MediaType != "" && m.rec.t.MediaType != q.MediaType {
			continue
		}
		if kw != "" && !matchKeyword(&m.rec.t, kw) {
			continue
		}
		hit = append(hit, m.rec)
	}
	total := len(hit)
	from := (q.Page - 1) * q.Size
	if from >= total {
		return ListResult{Total: total}
	}
	to := from + q.Size
	if to > total {
		to = total
	}
	return ListResult{Recs: hit[from:to], Total: total}
}

// Counts 范围内此刻可见模板的数量（只此一个范围）。
type Counts struct {
	Image      int
	Video      int
	NewCount7d int
}

func (c *Catalog) Counts(sc *Scope, now time.Time) Counts {
	var out Counts
	for _, m := range c.members(sc, now) {
		switch m.rec.t.MediaType {
		case model.MediaVideo:
			out.Video++
		default:
			out.Image++
		}
		if now.Sub(m.rec.t.CreateTime) <= 7*24*time.Hour {
			out.NewCount7d++
		}
	}
	return out
}

// Get 按 id 取单条（深链还原 / 做同款准备 / 报价）：只返回此刻可见、且至少挂在一个有效（启用、父级也启用）
// 行业下的模板，其余 ErrTemplateGone——没有行业归属的模板在用户端任何浏览入口都看不到，按 id 也不放出来。
func (c *Catalog) Get(id int64, now time.Time) (*tplRec, error) {
	rec := c.tpls[id]
	if rec == nil || !rec.visibleAt(now) || !c.hasEffectiveIndustry(rec) {
		return nil, ErrTemplateGone()
	}
	return rec, nil
}

// hasEffectiveIndustry 模板至少有一条指向有效行业的关系。
func (c *Catalog) hasEffectiveIndustry(rec *tplRec) bool {
	for _, r := range rec.rels {
		if c.byID[r.IndustryID].effective() {
			return true
		}
	}
	return false
}

// InScope 模板是否属于范围（用于报价 / 示例素材 / 最近用过的行业过滤），且此刻可见。
func (c *Catalog) InScope(sc *Scope, id int64, now time.Time) (*tplRec, bool) {
	rec := c.tpls[id]
	if rec == nil || !rec.visibleAt(now) {
		return nil, false
	}
	for _, r := range rec.rels {
		for _, sid := range sc.ids {
			if r.IndustryID == sid {
				return rec, true
			}
		}
	}
	return nil, false
}

// IndustryRef 模板所属行业的引用。
type IndustryRef struct {
	ID         int64  `json:"id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	ParentCode string `json:"parentCode,omitempty"`
}

// industriesOf 模板所属的有效（启用）行业，按行业顺序（一级在前、sort_order、id）。
func (c *Catalog) industriesOf(rec *tplRec) []IndustryRef {
	var ns []*IndustryNode
	for _, r := range rec.rels {
		if n := c.byID[r.IndustryID]; n != nil && n.effective() {
			ns = append(ns, n)
		}
	}
	sort.Slice(ns, func(i, j int) bool {
		a, b := ns[i], ns[j]
		ar, br := rootOf(a), rootOf(b)
		if ar.SortOrder != br.SortOrder {
			return ar.SortOrder < br.SortOrder
		}
		if ar.ID != br.ID {
			return ar.ID < br.ID
		}
		if (a.Parent == nil) != (b.Parent == nil) {
			return a.Parent == nil
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID < b.ID
	})
	out := make([]IndustryRef, 0, len(ns))
	for _, n := range ns {
		out = append(out, IndustryRef{ID: n.ID, Code: n.Code, Name: n.Name, ParentCode: n.parentCode()})
	}
	return out
}

func rootOf(n *IndustryNode) *IndustryNode {
	if n.Parent != nil {
		return n.Parent
	}
	return n
}

// ---------------------------------------------------------------- 行业目录

// DirectoryChild 二级。
type DirectoryChild struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	Hint string `json:"hint,omitempty"`
}

// DirectoryItem 目录里的一级行业。只含行业本身，没有任何数量与模板内容。
type DirectoryItem struct {
	ID           int64            `json:"id"`
	Code         string           `json:"code"`
	Name         string           `json:"name"`
	IconURL      *string          `json:"iconUrl"`
	Hint         string           `json:"hint"`
	AudienceName string           `json:"audienceName"`
	Examples     []string         `json:"examples"`
	NextActions  []string         `json:"nextActions"`
	Children     []DirectoryChild `json:"children"`
}

// Directory 用户端行业目录：只返回启用、未删除，且至少有一个此刻可见的模板（或 show_when_empty=1）的行业；
// 一级自身没有模板但有非空子级则保留；停用的子级不出现在 children；父级停用整棵不可见。
func (c *Catalog) Directory(now time.Time) []DirectoryItem {
	out := []DirectoryItem{}
	for _, r := range c.roots {
		if !r.effective() {
			continue
		}
		var kids []DirectoryChild
		anyKid := false
		for _, k := range r.Children {
			if !k.effective() {
				continue
			}
			if c.industryShown(k, now) {
				anyKid = true
				kids = append(kids, DirectoryChild{ID: k.ID, Code: k.Code, Name: k.Name, Hint: k.Hint.String})
			}
		}
		if !c.industryShown(r, now) && !anyKid {
			continue
		}
		it := DirectoryItem{
			ID: r.ID, Code: r.Code, Name: r.Name, Hint: r.Hint.String, AudienceName: r.AudienceName.String,
			Examples: jsonStrings(r.ExamplesJSON), NextActions: jsonStrings(r.NextActionsJSON),
			Children: kids,
		}
		if it.Children == nil {
			it.Children = []DirectoryChild{}
		}
		if r.IconURL.Valid && r.IconURL.String != "" {
			s := r.IconURL.String
			it.IconURL = &s
		}
		out = append(out, it)
	}
	return out
}

// industryShown 行业自己（一级含其全部子级）是否有此刻可见的模板，或开了 show_when_empty。
func (c *Catalog) industryShown(n *IndustryNode, now time.Time) bool {
	if n.ShowWhenEmpty == 1 {
		return true
	}
	return len(c.members(c.scopeOf(n), now)) > 0
}

// shownInDirectory 某行业此刻是否会出现在用户端行业目录里（后台展示「用户端不可见」徽标用，与 Directory 同一套规则）。
func (c *Catalog) shownInDirectory(id int64, now time.Time) bool {
	n := c.byID[id]
	if n == nil || !n.effective() {
		return false
	}
	if c.industryShown(n, now) {
		return true
	}
	for _, k := range n.Children {
		if k.effective() && c.industryShown(k, now) {
			return true
		}
	}
	return false
}

// Lookup 按 code 找有效行业（偏好等用）。
func (c *Catalog) Lookup(code string) *IndustryNode {
	n := c.byCode[strings.TrimSpace(code)]
	if n == nil || !n.effective() {
		return nil
	}
	return n
}

// LookupID 按 id 找有效行业。
func (c *Catalog) LookupID(id int64) *IndustryNode {
	n := c.byID[id]
	if n == nil || !n.effective() {
		return nil
	}
	return n
}

// ---------------------------------------------------------------- 关键词

// MaxKeywordRunes 搜索词上限（5.1：≤50 字）。
const MaxKeywordRunes = 50

func normKeyword(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func matchKeyword(t *model.Template, kw string) bool {
	if strings.Contains(strings.ToLower(t.Title), kw) {
		return true
	}
	desc := t.Description.String
	if desc == "" {
		desc = topString(t.DataJSON, "description")
	}
	if strings.Contains(strings.ToLower(desc), kw) {
		return true
	}
	for _, tag := range readTags(t.TagsJSON) {
		if strings.Contains(strings.ToLower(tag), kw) {
			return true
		}
	}
	return false
}

func validKeyword(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxKeywordRunes {
		return "", errParam("搜索词最多 50 个字")
	}
	return s, nil
}
