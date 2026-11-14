package service

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/xerr"
)

// staleAfter 超过 3 个刷新周期没更新视为陈旧（管理端变红）。
func (s *Service) staleAfter() time.Duration { return 3 * s.conf.refreshInterval() }

// TemplateBrief 管理端条目里的模板摘要。
type TemplateBrief struct {
	ID        int64    `json:"id"`
	Title     string   `json:"title"`
	Cover     string   `json:"cover"`
	MediaType string   `json:"mediaType"`
	Tags      []string `json:"tags"`
	Visible   bool     `json:"visible"`
	Exists    bool     `json:"exists"`
}

func brief(cat *store.Catalog, id int64, now time.Time) TemplateBrief {
	t, ok := cat.Templates[id]
	if !ok {
		return TemplateBrief{ID: id, Tags: []string{}}
	}
	tags := t.Tags
	if tags == nil {
		tags = []string{}
	}
	return TemplateBrief{ID: id, Title: t.Title, Cover: t.Cover(), MediaType: t.MediaType, Tags: tags, Visible: t.VisibleAt(now), Exists: true}
}

// AdminItem 管理端条目：完整算法条目（含 source / score / score_detail）+ 模板摘要 + 人工规则 + 当前是否被抑制。
type AdminItem struct {
	algo.Item
	Template     TemplateBrief       `json:"template"`
	Rule         *store.HeadlineRule `json:"rule,omitempty"`
	RuleStart    *string             `json:"ruleStartDate,omitempty"`
	RuleEnd      *string             `json:"ruleEndDate,omitempty"`
	Suppressed   bool                `json:"suppressed"`
	UsageVisible bool                `json:"usageVisible"`
}

// SnapshotMeta 管理端版本元信息。
type SnapshotMeta struct {
	Version       int64   `json:"version"`
	ComputedAt    string  `json:"computedAt"`
	AgeMinutes    int     `json:"ageMinutes"`
	Trigger       string  `json:"trigger"`
	OperatorID    *int64  `json:"operatorId"`
	SignalSource  string  `json:"signalSource"`
	ParamsHash    string  `json:"paramsHash"`
	Candidates    int     `json:"candidateCount"`
	ScopeCount    int     `json:"scopeCount"`
	ItemCount     int     `json:"itemCount"`
	DurationMs    *int    `json:"durationMs"`
	Stale         bool    `json:"stale"`
	WindowStart   string  `json:"windowStart"`
	WindowEnd     string  `json:"windowEnd"`
	ParamsJSONRaw *string `json:"params,omitempty"`
}

func (s *Service) meta(snap *store.Snapshot, withParams bool) SnapshotMeta {
	now := s.now()
	m := SnapshotMeta{Version: snap.ID, ComputedAt: fmtTime(snap.ComputedAt.Time), AgeMinutes: int(now.Sub(snap.ComputedAt.Time).Minutes()),
		Trigger: snap.TriggerType, OperatorID: snap.OperatorID, SignalSource: snap.SignalSource, ParamsHash: snap.ParamsHash,
		Candidates: snap.CandidateCount, ScopeCount: snap.ScopeCount, ItemCount: snap.ItemCount, DurationMs: snap.DurationMs,
		Stale: now.Sub(snap.ComputedAt.Time) > s.staleAfter(), WindowStart: fmtTime(snap.WindowStart.Time), WindowEnd: fmtTime(snap.WindowEnd.Time)}
	if withParams && len(snap.ParamsJSON) > 0 {
		v := string(snap.ParamsJSON)
		m.ParamsJSONRaw = &v
	}
	return m
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(algo.CST).Format("2006-01-02T15:04:05")
}

func fmtDate(t time.Time) string { return t.In(algo.CST).Format("2006-01-02") }

// ScopeOverview GET /scopes 的一行。
type ScopeOverview struct {
	Scope      string `json:"scope"`
	Name       string `json:"name"`
	ParentCode string `json:"parentCode,omitempty"`
	Members    int    `json:"memberCount"`
	Items      int    `json:"itemCount"`
	Auto       int    `json:"auto"`
	Manual     int    `json:"manual"`
	Fill       int    `json:"fill"`
	ColdStart  bool   `json:"coldStart"` // FILL>0：合格模板不足，正用本行业最新上架补位
	Computed   bool   `json:"computed"`  // 最新版本里是否有这个范围（新行业要等下一次刷新）
}

// ScopesView GET /scopes：行业切换器的数据源（ALL 在首位）。
type ScopesView struct {
	Snapshot *SnapshotMeta   `json:"snapshot"`
	Scopes   []ScopeOverview `json:"scopes"`
}

// Scopes 范围列表与概况。
func (s *Service) Scopes(ctx context.Context) (*ScopesView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := s.repo.LatestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	stats := map[string]algo.ScopeStats{}
	counts := map[string]int{}
	out := &ScopesView{Scopes: []ScopeOverview{}}
	if snap != nil {
		m := s.meta(snap, false)
		out.Snapshot = &m
		_ = json.Unmarshal(snap.ScopeStats, &stats)
		rows, err := s.repo.Items(ctx, snap.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			counts[r.ScopeCode]++
		}
	}
	row := func(code, name, parent string, members int) ScopeOverview {
		st, ok := stats[code]
		return ScopeOverview{Scope: code, Name: name, ParentCode: parent, Members: members, Items: counts[code],
			Auto: st.Auto, Manual: st.Manual, Fill: st.Fill, ColdStart: st.Fill > 0, Computed: ok}
	}
	out.Scopes = append(out.Scopes, row(algo.ScopeAll, "全站总榜", "", len(cat.Templates)))
	for _, sc := range cat.Scopes {
		out.Scopes = append(out.Scopes, row(sc.Code, sc.Name, sc.ParentCode, len(sc.Members)))
	}
	return out, nil
}

// ScopeView 某版本某范围的完整榜单（当前榜单 / 历史回看）。
type ScopeView struct {
	Snapshot   SnapshotMeta     `json:"snapshot"`
	Scope      string           `json:"scope"`
	Items      []AdminItem      `json:"items"`
	NearMiss   []NearMissView   `json:"nearMiss"`
	Skip       map[string]int   `json:"skip"`
	Thresholds algo.Thresholds  `json:"thresholds"`
	Stats      *algo.ScopeStats `json:"stats,omitempty"`
}

// NearMissView 近邻候选 + 模板摘要。
type NearMissView struct {
	algo.NearMiss
	Template TemplateBrief `json:"template"`
}

func errSnapshotExpired() error {
	return xerr.NewReason(xerr.NotFound, ReasonSnapshotExpired, "该版本已超过保留期")
}

// Current 当前榜单（scope 缺省 ALL）。
func (s *Service) Current(ctx context.Context, scope string) (*ScopeView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	snap, err := s.repo.LatestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, xerr.NewReason(xerr.NotFound, ReasonSnapshotExpired, "还没有生成过榜单，请先立即刷新")
	}
	return s.scopeView(ctx, snap, scope)
}

// SnapshotScope 某版本某范围。
func (s *Service) SnapshotScope(ctx context.Context, version int64, scope string) (*ScopeView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	snap, err := s.repo.SnapshotByID(ctx, version)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errSnapshotExpired()
	}
	return s.scopeView(ctx, snap, scope)
}

// SnapshotAt 时刻 t 生效的版本的某范围。
func (s *Service) SnapshotAt(ctx context.Context, at time.Time, scope string) (*ScopeView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	snap, err := s.repo.SnapshotAt(ctx, at)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errSnapshotExpired()
	}
	return s.scopeView(ctx, snap, scope)
}

func (s *Service) scopeView(ctx context.Context, snap *store.Snapshot, scope string) (*ScopeView, error) {
	if scope == "" {
		scope = algo.ScopeAll
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.Items(ctx, snap.ID, scope)
	if err != nil {
		return nil, err
	}
	rules, blocks, err := s.rulesAndBlocks(ctx, false)
	if err != nil {
		return nil, err
	}
	var ab []algo.Block
	for _, b := range blocks {
		ab = append(ab, b.Algo())
	}
	now := s.now()
	suppressed := algo.SuppressedSet(scope, ab, now)
	ruleByID := map[int64]store.HeadlineRule{}
	for _, r := range rules {
		ruleByID[r.ID] = r
	}
	cfg, _, _ := s.LoadConfig(ctx)
	p := cfg.For(scope)
	view := &ScopeView{Snapshot: s.meta(snap, true), Scope: scope, Items: []AdminItem{}, NearMiss: []NearMissView{}, Skip: map[string]int{}}
	for _, r := range rows {
		it := r.Algo()
		ai := AdminItem{Item: it, Template: brief(cat, it.TemplateID, now), Suppressed: suppressed[it.TemplateID], UsageVisible: algo.UsageVisible(it.Uses7d, p)}
		if it.HeadlineID != 0 {
			if rule, ok := ruleByID[it.HeadlineID]; ok {
				rr := rule
				ai.Rule = &rr
				sd, ed := fmtDate(rule.StartDate.Time), fmtDate(rule.EndDate.Time)
				ai.RuleStart, ai.RuleEnd = &sd, &ed
			}
		}
		view.Items = append(view.Items, ai)
	}
	var stats map[string]algo.ScopeStats
	if json.Unmarshal(snap.ScopeStats, &stats) == nil {
		if st, ok := stats[scope]; ok {
			view.Skip, view.Thresholds = st.Skip, st.Thresholds
			stCopy := st
			stCopy.NearMiss = nil
			view.Stats = &stCopy
			for _, nm := range st.NearMiss {
				view.NearMiss = append(view.NearMiss, NearMissView{NearMiss: nm, Template: brief(cat, nm.TemplateID, now)})
			}
		}
	}
	return view, nil
}

// CompareView 并排对比：多个范围的榜单，按模板对齐。
type CompareView struct {
	Snapshot SnapshotMeta           `json:"snapshot"`
	Scopes   []string               `json:"scopes"`
	Rows     []CompareRow           `json:"rows"`
	Lists    map[string][]algo.Item `json:"lists"`
}

// CompareRow 一个模板在各范围的名次 / 来源 / 分数。
type CompareRow struct {
	Template TemplateBrief           `json:"template"`
	Cells    map[string]*CompareCell `json:"cells"` // 不在该榜为 null
	Scopes   []string                `json:"memberOf"`
}

// CompareCell 对比格。
type CompareCell struct {
	Rank   int      `json:"rank"`
	Source string   `json:"source"`
	Score  *float64 `json:"score"`
	Uses7d int      `json:"uses7d"`
}

// Compare 一次返回 2~4 个范围（建议含 ALL）的榜单并按模板对齐；at 非零时回看该时刻生效的版本。
func (s *Service) Compare(ctx context.Context, scopes []string, at time.Time) (*CompareView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	scopes = dedupe(scopes)
	if len(scopes) == 0 || len(scopes) > 4 {
		return nil, xerr.Param("scopes 需要 1~4 个范围")
	}
	var snap *store.Snapshot
	var err error
	if at.IsZero() {
		snap, err = s.repo.LatestSnapshot(ctx)
	} else {
		snap, err = s.repo.SnapshotAt(ctx, at)
	}
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errSnapshotExpired()
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.Items(ctx, snap.ID, scopes...)
	if err != nil {
		return nil, err
	}
	now := s.now()
	view := &CompareView{Snapshot: s.meta(snap, false), Scopes: scopes, Rows: []CompareRow{}, Lists: map[string][]algo.Item{}}
	byTpl := map[int64]*CompareRow{}
	var order []int64
	for _, r := range rows {
		it := r.Algo()
		it.Detail = nil
		view.Lists[r.ScopeCode] = append(view.Lists[r.ScopeCode], it)
		row, ok := byTpl[it.TemplateID]
		if !ok {
			row = &CompareRow{Template: brief(cat, it.TemplateID, now), Cells: map[string]*CompareCell{}, Scopes: cat.ScopesOf(it.TemplateID)}
			for _, sc := range scopes {
				row.Cells[sc] = nil
			}
			if row.Scopes == nil {
				row.Scopes = []string{}
			}
			byTpl[it.TemplateID] = row
			order = append(order, it.TemplateID)
		}
		row.Cells[r.ScopeCode] = &CompareCell{Rank: it.Rank, Source: it.Source, Score: it.Score, Uses7d: it.Uses7d}
	}
	// 按「在任一榜里的最好名次」排序，便于发现「全局很热但行业榜没有」
	best := func(id int64) int {
		b := 1 << 30
		for _, c := range byTpl[id].Cells {
			if c != nil && c.Rank < b {
				b = c.Rank
			}
		}
		return b
	}
	sort.SliceStable(order, func(i, j int) bool { return best(order[i]) < best(order[j]) })
	for _, id := range order {
		view.Rows = append(view.Rows, *byTpl[id])
	}
	return view, nil
}

// TemplateScopeStatus 模板在某个榜的情况。
type TemplateScopeStatus struct {
	Scope       string   `json:"scope"`
	Name        string   `json:"name"`
	OnBoard     bool     `json:"onBoard"`
	Rank        *int     `json:"rank"`
	Source      string   `json:"source,omitempty"`
	Score       *float64 `json:"score"`
	Uses7d      int      `json:"uses7d"`
	Suppressed  bool     `json:"suppressed"`
	NearMiss    bool     `json:"nearMiss"`
	SkipReason  string   `json:"skipReason,omitempty"`
	GapToCutoff *float64 `json:"gapToCutoff"`
}

// TemplateStatusView GET /templates/{id}/headline-status。
type TemplateStatusView struct {
	Template TemplateBrief         `json:"template"`
	Version  int64                 `json:"version"`
	Scopes   []TemplateScopeStatus `json:"scopes"` // 所属行业榜 + ALL（最后）
	Rules    []RuleView            `json:"rules"`
	Blocks   []BlockView           `json:"blocks"`
}

// TemplateStatus 模板在每个所属行业榜与 ALL 榜里的名次 / 来源 / 分数 / 抑制 / 离门槛的差距，及适用的规则与抑制。
func (s *Service) TemplateStatus(ctx context.Context, tplID int64) (*TemplateStatusView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := cat.Templates[tplID]; !ok {
		return nil, xerr.NewReason(xerr.NotFound, ReasonHeadlineTemplateAbsent, "模板不存在")
	}
	now := s.now()
	view := &TemplateStatusView{Template: brief(cat, tplID, now), Scopes: []TemplateScopeStatus{}, Rules: []RuleView{}, Blocks: []BlockView{}}
	scopeCodes := append(cat.ScopesOf(tplID), algo.ScopeAll)
	snap, err := s.repo.LatestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	onBoard := map[string]store.SnapshotItem{}
	stats := map[string]algo.ScopeStats{}
	if snap != nil {
		view.Version = snap.ID
		items, err := s.repo.ItemsOfTemplate(ctx, snap.ID, tplID)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			onBoard[it.ScopeCode] = it
		}
		_ = json.Unmarshal(snap.ScopeStats, &stats)
	}
	blocks, err := s.repo.ListBlocks(ctx, "", tplID, true, now)
	if err != nil {
		return nil, err
	}
	var ab []algo.Block
	for _, b := range blocks {
		ab = append(ab, b.Algo())
		view.Blocks = append(view.Blocks, blockView(b, cat))
	}
	for _, code := range scopeCodes {
		st := TemplateScopeStatus{Scope: code, Name: scopeName(cat, code)}
		if code != algo.ScopeAll {
			st.Suppressed = algo.SuppressedSet(code, ab, now)[tplID]
		}
		if it, ok := onBoard[code]; ok {
			r := it.RankNo
			st.OnBoard, st.Rank, st.Source, st.Score, st.Uses7d = true, &r, it.Source, it.Score, it.Uses7d
		} else if ss, ok := stats[code]; ok {
			for _, nm := range ss.NearMiss {
				if nm.TemplateID == tplID {
					g := nm.Gap
					st.NearMiss, st.SkipReason, st.GapToCutoff, st.Uses7d = true, nm.Reason, &g, nm.Uses7d
				}
			}
		}
		view.Scopes = append(view.Scopes, st)
	}
	rules, err := s.repo.RulesOfTemplate(ctx, tplID)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		view.Rules = append(view.Rules, ruleView(r, cat, now))
	}
	return view, nil
}

func scopeName(cat *store.Catalog, code string) string {
	switch code {
	case algo.ScopeAll:
		return "全站总榜"
	case algo.ScopeAny:
		return "所有适用行业"
	}
	if sc, ok := cat.Scope(code); ok {
		return sc.Name
	}
	if ind, ok := cat.Industry(code); ok {
		return ind.Name
	}
	return code
}

// RuleView 管理端人工规则行。
type RuleView struct {
	store.HeadlineRule
	StartDate string        `json:"startDate"`
	EndDate   string        `json:"endDate"`
	ScopeName string        `json:"scopeName"`
	Phase     string        `json:"phase"` // CURRENT | UPCOMING | EXPIRED
	Template  TemplateBrief `json:"template"`
}

func ruleView(r store.HeadlineRule, cat *store.Catalog, now time.Time) RuleView {
	today := fmtDate(now)
	sd, ed := fmtDate(r.StartDate.Time), fmtDate(r.EndDate.Time)
	phase := "CURRENT"
	switch {
	case sd > today:
		phase = "UPCOMING"
	case ed < today:
		phase = "EXPIRED"
	}
	return RuleView{HeadlineRule: r, StartDate: sd, EndDate: ed, ScopeName: scopeName(cat, r.ScopeCode), Phase: phase, Template: brief(cat, r.PlazaTemplateID, now)}
}

// BlockView 管理端抑制行。
type BlockView struct {
	store.Block
	UntilAt   *string `json:"untilAt"` // null = 永久
	Permanent bool    `json:"permanent"`
	ScopeName string  `json:"scopeName"`
	Title     string  `json:"templateTitle"`
}

func blockView(b store.Block, cat *store.Catalog) BlockView {
	v := BlockView{Block: b, Permanent: b.UntilAt.IsZero(), ScopeName: scopeName(cat, b.ScopeCode)}
	if !b.UntilAt.IsZero() {
		u := fmtTime(b.UntilAt.Time)
		v.UntilAt = &u
	}
	if t, ok := cat.Templates[b.PlazaTemplateID]; ok {
		v.Title = t.Title
	}
	return v
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
