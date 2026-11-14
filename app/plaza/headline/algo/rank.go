package algo

import (
	"math"
	"sort"
	"time"
)

// 条目来源。
const (
	SourceAuto   = "AUTO"   // 算法
	SourceManual = "MANUAL" // 运营人工推荐（含接管）
	SourceFill   = "FILL"   // 冷启动补位：同范围最新上架，无热度
)

// 排除原因码（按 14.5.3 的顺序判定，写入 scope_stats.skip 与 nearMiss）。
const (
	SkipNotInScope      = "NOT_IN_SCOPE"
	SkipDisabled        = "TEMPLATE_DISABLED"
	SkipSuppressed      = "SUPPRESSED"
	SkipManualExists    = "MANUAL_EXISTS"
	SkipBelowMinDecayed = "BELOW_MIN_DECAYED"
	SkipBelowMinUsers   = "BELOW_MIN_USERS"
	SkipDiversityMedia  = "DIVERSITY_MEDIA"
	SkipCutBySlots      = "CUT_BY_SLOTS"
)

// Template 目录里的一个模板（范围计算只关心这些字段）。
type Template struct {
	ID        int64
	MediaType string
	CreatedAt time.Time
	// Visible 对用户可见：已发布、在上下线窗口内、未删除、有封面。不可见的不参选、不补位、人工规则也失效。
	Visible bool
}

// ManualRule 人工推荐规则（afv_plaza_headline 一行）。
type ManualRule struct {
	ID          int64
	TemplateID  int64
	Scope       string // 行业 code | *
	StartDate   time.Time
	EndDate     time.Time
	Why         string
	SortOrder   int
	Enabled     bool
	PinPosition int // 0 = 未定位
	Adopted     bool
}

// ActiveOn 规则在 now（CST 自然日）是否生效：启用且 start_date ≤ 今天 ≤ end_date（日期闭区间）。
func (r ManualRule) ActiveOn(now time.Time) bool {
	if !r.Enabled {
		return false
	}
	today := dateOf(now)
	return !dateOf(r.StartDate).After(today) && !dateOf(r.EndDate).Before(today)
}

// Block 抑制规则（afv_plaza_headline_block 一行）：取消到某时刻 / 永久屏蔽。
type Block struct {
	ID         int64
	TemplateID int64
	Scope      string // 行业 code | *
	Kind       string // CANCEL | BLOCK
	Until      *time.Time
}

// ActiveAt 抑制在 now 是否生效：永久，或 until 晚于 now。
func (b Block) ActiveAt(now time.Time) bool { return b.Until == nil || b.Until.After(now) }

// AppliesTo 规则 / 抑制的范围是否作用于 scope：精确匹配或 *（* 只在包含该模板的范围生效，由调用方保证成员关系）。
func AppliesTo(ruleScope, scope string) bool {
	return scope != ScopeAll && (ruleScope == scope || ruleScope == ScopeAny)
}

// PrevItem 上一版快照中同一范围的条目（迟滞 / 驻留 / 名次限动的依据）。
type PrevItem struct {
	TemplateID int64
	Rank       int
	Source     string
	EnteredAt  time.Time
}

// ScopeInput 一个范围的计算输入。Templates / Stats 是全局共享的；Members 是该范围的成员。
type ScopeInput struct {
	Scope     string
	Members   []int64
	Templates map[int64]Template
	// Stats 有信号的模板的统计量（ComputeStats 产出）；没有信号的模板按零信号计算。
	Stats  map[int64]Stats
	Rules  []ManualRule
	Blocks []Block
	Prev   []PrevItem
	Params Params
	Now    time.Time
}

// Thresholds 该范围生效的入选门槛（相对阈值，14.5.3）。
type Thresholds struct {
	EffMinDecayed float64 `json:"effMinDecayed"`
	EffMinUsers   int     `json:"effMinUsers"`
}

// Stability 迟滞 / 驻留 / 名次限动的痕迹。
type Stability struct {
	Incumbent      bool    `json:"incumbent"`
	EffScore       float64 `json:"effScore"`
	EntryCutoff    float64 `json:"entryCutoff"`
	DwellProtected bool    `json:"dwellProtected"`
	RankMovedFrom  *int    `json:"rankMovedFrom"`
	RankClampedBy  int     `json:"rankClampedBy"`
}

// MediaCap 媒体类型上限的痕迹。
type MediaCap struct {
	MediaType  string `json:"mediaType"`
	MediaCount int    `json:"mediaCount"`
}

// ScoreDetail 条目的可解释明细（快照 score_detail，管理端悬停展示；用户端不下发）。
type ScoreDetail struct {
	Scope      string                  `json:"scope"`
	Signals    map[string]SignalDetail `json:"signals"`
	Stats      Stats                   `json:"stats"`
	Thresholds Thresholds              `json:"thresholds"`
	Stability  Stability               `json:"stability"`
	MediaCap   MediaCap                `json:"mediaCap"`
	PickedBy   string                  `json:"pickedBy"` // SCORE | DWELL
}

// Item 最终榜单的一条。
type Item struct {
	Scope       string       `json:"scope"`
	TemplateID  int64        `json:"plazaTemplateId"`
	Rank        int          `json:"rank"`
	Source      string       `json:"source"`
	HeadlineID  int64        `json:"headlineId,omitempty"`
	Score       *float64     `json:"score"`
	Detail      *ScoreDetail `json:"scoreDetail,omitempty"`
	Why         string       `json:"why"`
	Uses7d      int          `json:"uses7d"`
	PrevUses    int          `json:"prevUses"`
	Trend       *int         `json:"trend"`
	IsNew       bool         `json:"isNew"`
	UniqueUsers *int         `json:"uniqueUsers"`
	EnteredAt   time.Time    `json:"enteredAt"`
	PinPosition int          `json:"pinPosition,omitempty"`
	Adopted     bool         `json:"adopted,omitempty"`
}

// NearMiss 紧随入选项之后的候选（含排除原因与离门槛的差距）。
type NearMiss struct {
	TemplateID int64   `json:"plazaTemplateId"`
	Score      float64 `json:"score"`
	EffScore   float64 `json:"effScore"`
	Reason     string  `json:"reason"`
	Gap        float64 `json:"gap"` // 入榜门槛（末位有效分）− 本项有效分；无入选项时为 0
	Uses7d     int     `json:"uses7d"`
}

// ScopeStats 一个范围的计算摘要（快照 scope_stats）。
type ScopeStats struct {
	Candidates int            `json:"candidates"`
	Eligible   int            `json:"eligible"`
	Auto       int            `json:"auto"`
	Manual     int            `json:"manual"`
	Fill       int            `json:"fill"`
	AutoSlots  int            `json:"autoSlots"`
	Skip       map[string]int `json:"skip"`
	NearMiss   []NearMiss     `json:"nearMiss"`
	Thresholds Thresholds     `json:"thresholds"`
}

// ScopeResult 一个范围的最终榜单与摘要。
type ScopeResult struct {
	Scope string     `json:"scope"`
	Items []Item     `json:"items"`
	Stats ScopeStats `json:"stats"`
}

func dateOf(t time.Time) time.Time {
	y, m, d := t.In(CST).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, CST)
}

// statsFor 有信号取信号统计，否则按零信号算（新鲜度 / 年龄仍有意义）。
func statsFor(in *ScopeInput, id int64) Stats {
	if st, ok := in.Stats[id]; ok {
		return st
	}
	return ComputeStats(Signal{TemplateID: id}, in.Templates[id].CreatedAt, in.Now, in.Params)
}

// EffectiveManual 该范围生效的人工项（14.7-1）：范围 ∈ {S,*}、启用、窗口含今天、模板属于 S 且可见、不在 S 的抑制名单。
// 同模板多行：显式范围优先于 *，再按起始日最晚 → sort_order 升序 → id 降序（同 Java dedupeByTemplate）。
func EffectiveManual(scope string, members map[int64]bool, templates map[int64]Template, rules []ManualRule, suppressed map[int64]bool, now time.Time) []ManualRule {
	best := map[int64]ManualRule{}
	better := func(a, b ManualRule) bool { // a 是否优于 b
		ae, be := a.Scope != ScopeAny, b.Scope != ScopeAny
		if ae != be {
			return ae
		}
		if !a.StartDate.Equal(b.StartDate) {
			return a.StartDate.After(b.StartDate)
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID > b.ID
	}
	for _, r := range rules {
		if !AppliesTo(r.Scope, scope) || !r.ActiveOn(now) || !members[r.TemplateID] || suppressed[r.TemplateID] {
			continue
		}
		if t, ok := templates[r.TemplateID]; !ok || !t.Visible {
			continue
		}
		if cur, ok := best[r.TemplateID]; !ok || better(r, cur) {
			best[r.TemplateID] = r
		}
	}
	out := make([]ManualRule, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SuppressedSet 该范围在 now 生效的抑制（范围 ∈ {S,*}，永久或未到期）。
func SuppressedSet(scope string, blocks []Block, now time.Time) map[int64]bool {
	out := map[int64]bool{}
	for _, b := range blocks {
		if AppliesTo(b.Scope, scope) && b.ActiveAt(now) {
			out[b.TemplateID] = true
		}
	}
	return out
}

type cand struct {
	id        int64
	st        Stats
	score     float64
	eff       float64
	detail    map[string]SignalDetail
	incumbent bool
	prevPos   int
	protected bool
	reason    string
	media     string
}

// less 有效分降序 → uses7d 降序 → 模板 id 降序（14.5.4-3 的同分规则）。
func less(a, b *cand) bool {
	if a.eff != b.eff {
		return a.eff > b.eff
	}
	if a.st.Uses7d != b.st.Uses7d {
		return a.st.Uses7d > b.st.Uses7d
	}
	return a.id > b.id
}

// ComputeScope 一个行业范围的完整计算：资格 → 打分 → 迟滞 / 驻留 → 媒体上限 → 名次限动 → 冷启动补位 → 合并人工项。
func ComputeScope(in ScopeInput) ScopeResult {
	p := in.Params
	now := in.Now
	res := ScopeResult{Scope: in.Scope, Stats: ScopeStats{Skip: map[string]int{}}}
	members := make(map[int64]bool, len(in.Members))
	for _, id := range in.Members {
		members[id] = true
	}
	suppressed := SuppressedSet(in.Scope, in.Blocks, now)
	manual := EffectiveManual(in.Scope, members, in.Templates, in.Rules, suppressed, now)
	manualSet := make(map[int64]ManualRule, len(manual))
	for _, r := range manual {
		manualSet[r.TemplateID] = r
	}
	autoSlots := p.TopN
	if v := p.MaxVisible - len(manual); v < autoSlots {
		autoSlots = v
	}
	if autoSlots < 0 {
		autoSlots = 0
	}
	res.Stats.AutoSlots = autoSlots

	// 上一版：AUTO 条目是在榜项（按名次得到它们在 AUTO 序列中的位置）；任何来源都结转连续在榜起点
	prevEntered := map[int64]time.Time{}
	var prevAuto []PrevItem
	for _, it := range in.Prev {
		prevEntered[it.TemplateID] = it.EnteredAt
		if it.Source == SourceAuto {
			prevAuto = append(prevAuto, it)
		}
	}
	sort.Slice(prevAuto, func(i, j int) bool {
		if prevAuto[i].Rank != prevAuto[j].Rank {
			return prevAuto[i].Rank < prevAuto[j].Rank
		}
		return prevAuto[i].TemplateID < prevAuto[j].TemplateID
	})
	prevPos := make(map[int64]int, len(prevAuto))
	for i, it := range prevAuto {
		prevPos[it.TemplateID] = i + 1
	}

	// 候选 = 有信号的模板 ∪ 上一版在榜项；按 id 排序保证确定性
	idSet := map[int64]bool{}
	for id := range in.Stats {
		idSet[id] = true
	}
	for id := range prevPos {
		idSet[id] = true
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var pool []*cand // 通过资格 1~4 的
	for _, id := range ids {
		_, hasSignal := in.Stats[id]
		reason := ""
		switch {
		case !members[id]:
			reason = SkipNotInScope
		case !in.Templates[id].Visible:
			reason = SkipDisabled
		case suppressed[id]:
			reason = SkipSuppressed
		case hasManual(manualSet, id):
			reason = SkipManualExists
		}
		if reason != "" {
			if hasSignal {
				res.Stats.Skip[reason]++
			}
			continue
		}
		if hasSignal {
			res.Stats.Candidates++
		}
		pp, inc := prevPos[id]
		pool = append(pool, &cand{id: id, st: statsFor(&in, id), incumbent: inc, prevPos: pp, media: in.Templates[id].MediaType})
	}

	// 相对阈值：大行业受绝对阈值约束，小行业按自己的量级筛选
	maxDecayed, maxUsers := 0.0, 0
	for _, c := range pool {
		maxDecayed = math.Max(maxDecayed, c.st.Decayed)
		if c.st.UniqueUsers > maxUsers {
			maxUsers = c.st.UniqueUsers
		}
	}
	th := Thresholds{
		EffMinDecayed: round4(math.Min(p.MinDecayed, p.RelMinRatio*maxDecayed)),
		EffMinUsers:   minInt(p.MinUniqueUsers, maxInt(1, int(math.Ceil(p.RelMinRatio*float64(maxUsers)-1e-9)))),
	}
	res.Stats.Thresholds = th
	sc := ScoreContext{MaxDecayed: maxDecayed}
	dwell := time.Duration(p.MinDwellMinutes) * time.Minute
	keep := 1 - p.HysteresisDelta
	if keep <= 0 {
		keep = 1
	}
	var protected, eligible []*cand
	for _, c := range pool {
		c.score, c.detail = Score(c.st, p, sc)
		c.eff = c.score
		if c.incumbent {
			c.eff = round4(c.score / keep)
			if dwell > 0 {
				if ent, ok := prevEntered[c.id]; ok && now.Sub(ent) < dwell {
					c.protected = true // 驻留期内且仍满足资格 1~4：强制保留，不看阈值
				}
			}
		}
		switch {
		case c.st.Decayed < th.EffMinDecayed:
			c.reason = SkipBelowMinDecayed
		case c.st.UniqueUsers < th.EffMinUsers:
			c.reason = SkipBelowMinUsers
		}
		if c.protected {
			protected = append(protected, c)
			continue
		}
		if c.reason != "" {
			res.Stats.Skip[c.reason]++
			continue
		}
		eligible = append(eligible, c)
	}
	res.Stats.Eligible = len(eligible) + len(protected)
	sort.Slice(protected, func(i, j int) bool { return less(protected[i], protected[j]) })
	sort.Slice(eligible, func(i, j int) bool { return less(eligible[i], eligible[j]) })

	maxMedia := minInt(p.MaxPerMedia, autoSlots)
	mediaCount := map[string]int{}
	var selected []*cand
	pickedBy := map[int64]string{}
	for _, c := range protected {
		if len(selected) >= autoSlots {
			c.reason = SkipCutBySlots
			res.Stats.Skip[c.reason]++
			continue
		}
		c.reason = ""
		selected = append(selected, c)
		mediaCount[c.media]++ // 驻留项也占媒体名额，但不会被媒体上限挤掉
		pickedBy[c.id] = "DWELL"
	}
	for _, c := range eligible {
		switch {
		case len(selected) >= autoSlots:
			c.reason = SkipCutBySlots
		case mediaCount[c.media] >= maxMedia:
			c.reason = SkipDiversityMedia // 不递补放宽
		default:
			selected = append(selected, c)
			mediaCount[c.media]++
			pickedBy[c.id] = "SCORE"
			continue
		}
		res.Stats.Skip[c.reason]++
	}

	// 名次限动：按有效分得到目标位置，在榜项夹到 [prev−M, prev+M]，再按 (夹后位置, 有效分, id) 排序
	byEff := append([]*cand(nil), selected...)
	sort.Slice(byEff, func(i, j int) bool { return less(byEff[i], byEff[j]) })
	target := make(map[int64]int, len(byEff))
	clampedBy := map[int64]int{}
	for i, c := range byEff {
		t := i + 1
		if c.incumbent && p.MaxRankMove > 0 {
			ct := maxInt(c.prevPos-p.MaxRankMove, minInt(c.prevPos+p.MaxRankMove, t))
			clampedBy[c.id] = t - ct
			t = ct
		}
		target[c.id] = t
	}
	sort.SliceStable(byEff, func(i, j int) bool {
		a, b := byEff[i], byEff[j]
		if target[a.id] != target[b.id] {
			return target[a.id] < target[b.id]
		}
		// 同一目标位：被限住下滑的在榜项优先（保证它最多掉 M 名），被限住上升的排后，其余按有效分
		if pa, pb := tiePriority(clampedBy[a.id]), tiePriority(clampedBy[b.id]); pa != pb {
			return pa < pb
		}
		return less(a, b)
	})
	cutoff := 0.0
	for i, c := range byEff {
		if i == 0 || c.eff < cutoff {
			cutoff = c.eff
		}
	}

	// 近邻候选：未入选里有效分最高的 10 个
	var misses []*cand
	selSet := map[int64]bool{}
	for _, c := range byEff {
		selSet[c.id] = true
	}
	for _, c := range pool {
		if !selSet[c.id] {
			misses = append(misses, c)
		}
	}
	sort.Slice(misses, func(i, j int) bool { return less(misses[i], misses[j]) })
	for i, c := range misses {
		if i >= 10 {
			break
		}
		gap := 0.0
		if len(byEff) > 0 {
			gap = round4(cutoff - c.eff)
		}
		res.Stats.NearMiss = append(res.Stats.NearMiss, NearMiss{TemplateID: c.id, Score: c.score, EffScore: c.eff, Reason: c.reason, Gap: gap, Uses7d: c.st.Uses7d})
	}

	entered := func(id int64) time.Time {
		if t, ok := prevEntered[id]; ok && !t.IsZero() {
			return t
		}
		return now
	}
	var autoItems []Item
	mediaSeen := map[string]int{}
	for _, c := range byEff {
		mediaSeen[c.media]++
		score := c.score
		uu := c.st.UniqueUsers
		var movedFrom *int
		if c.incumbent {
			v := c.prevPos
			movedFrom = &v
		}
		autoItems = append(autoItems, Item{
			Scope: in.Scope, TemplateID: c.id, Source: SourceAuto, Score: &score,
			Detail: &ScoreDetail{
				Scope: in.Scope, Signals: c.detail, Stats: c.st, Thresholds: th,
				Stability: Stability{Incumbent: c.incumbent, EffScore: c.eff, EntryCutoff: cutoff, DwellProtected: c.protected,
					RankMovedFrom: movedFrom, RankClampedBy: clampedBy[c.id]},
				MediaCap: MediaCap{MediaType: c.media, MediaCount: mediaSeen[c.media]},
				PickedBy: pickedBy[c.id],
			},
			Why: AutoWhy(c.st, p), Uses7d: c.st.Uses7d, PrevUses: c.st.PrevUses, Trend: c.st.Trend(), IsNew: c.st.IsNew(),
			UniqueUsers: &uu, EnteredAt: entered(c.id),
		})
	}

	// 冷启动补位：只从同一范围的最新上架补，绝不跨行业；同样受媒体上限约束
	var fillItems []Item
	if p.ColdStartFill && len(autoItems) < autoSlots {
		var fillPool []Template
		for _, id := range in.Members {
			t, ok := in.Templates[id]
			if !ok || !t.Visible || suppressed[id] || hasManual(manualSet, id) || selSet[id] {
				continue
			}
			fillPool = append(fillPool, t)
		}
		sort.Slice(fillPool, func(i, j int) bool {
			if !fillPool[i].CreatedAt.Equal(fillPool[j].CreatedAt) {
				return fillPool[i].CreatedAt.After(fillPool[j].CreatedAt)
			}
			return fillPool[i].ID > fillPool[j].ID
		})
		seen := map[int64]bool{}
		for _, t := range fillPool {
			if len(autoItems)+len(fillItems) >= autoSlots {
				break
			}
			if seen[t.ID] || mediaCount[t.MediaType] >= maxMedia {
				continue
			}
			seen[t.ID] = true
			mediaCount[t.MediaType]++
			fillItems = append(fillItems, Item{Scope: in.Scope, TemplateID: t.ID, Source: SourceFill, Why: WhyFill, IsNew: true, EnteredAt: entered(t.ID)})
		}
	}

	// 人工项
	var manualItems []Item
	for _, r := range manual {
		st := statsFor(&in, r.TemplateID)
		uu := st.UniqueUsers
		manualItems = append(manualItems, Item{
			Scope: in.Scope, TemplateID: r.TemplateID, Source: SourceManual, HeadlineID: r.ID, Why: r.Why,
			Uses7d: st.Uses7d, PrevUses: st.PrevUses, Trend: st.Trend(), IsNew: st.IsNew(), UniqueUsers: &uu,
			EnteredAt: entered(r.TemplateID), PinPosition: r.PinPosition, Adopted: r.Adopted,
		})
	}
	res.Items = Place(manualItems, manual, autoItems, fillItems)
	res.Stats.Auto, res.Stats.Manual, res.Stats.Fill = len(autoItems), len(manualItems), len(fillItems)
	return res
}

// Place 摆放（14.7-2）：先把有定位的人工项放到各自名次（冲突按 id 顺延，越界放末位并向前找空位），
// 其余空位依次填 无定位人工项（uses7d 降序 → sort_order → id）→ AUTO → FILL。返回带最终名次的条目。
func Place(manualItems []Item, rules []ManualRule, autoItems, fillItems []Item) []Item {
	total := len(manualItems) + len(autoItems) + len(fillItems)
	if total == 0 {
		return []Item{}
	}
	ruleOf := map[int64]ManualRule{}
	for _, r := range rules {
		ruleOf[r.ID] = r
	}
	slots := make([]*Item, total)
	var pinned, free []*Item
	for i := range manualItems {
		it := &manualItems[i]
		if it.PinPosition > 0 {
			pinned = append(pinned, it)
		} else {
			free = append(free, it)
		}
	}
	sort.Slice(pinned, func(i, j int) bool {
		if pinned[i].PinPosition != pinned[j].PinPosition {
			return pinned[i].PinPosition < pinned[j].PinPosition
		}
		return pinned[i].HeadlineID < pinned[j].HeadlineID
	})
	for _, it := range pinned {
		pos := minInt(it.PinPosition, total) - 1
		placed := false
		for k := pos; k < total; k++ {
			if slots[k] == nil {
				slots[k], placed = it, true
				break
			}
		}
		for k := pos - 1; !placed && k >= 0; k-- {
			if slots[k] == nil {
				slots[k], placed = it, true
			}
		}
	}
	sort.Slice(free, func(i, j int) bool {
		a, b := free[i], free[j]
		if a.Uses7d != b.Uses7d {
			return a.Uses7d > b.Uses7d
		}
		ra, rb := ruleOf[a.HeadlineID], ruleOf[b.HeadlineID]
		if ra.SortOrder != rb.SortOrder {
			return ra.SortOrder < rb.SortOrder
		}
		return a.HeadlineID < b.HeadlineID
	})
	queue := append([]*Item{}, free...)
	for i := range autoItems {
		queue = append(queue, &autoItems[i])
	}
	for i := range fillItems {
		queue = append(queue, &fillItems[i])
	}
	qi := 0
	for k := 0; k < total && qi < len(queue); k++ {
		if slots[k] == nil {
			slots[k] = queue[qi]
			qi++
		}
	}
	out := make([]Item, 0, total)
	for k, it := range slots {
		if it == nil {
			continue
		}
		cp := *it
		cp.Rank = k + 1
		out = append(out, cp)
	}
	return out
}

// GlobalInput 后台全局总榜（ALL）的输入。
type GlobalInput struct {
	Templates map[int64]Template
	Stats     map[int64]Stats
	Prev      []PrevItem
	Params    Params
	Now       time.Time
}

// ComputeGlobal 全局总榜：纯热度原始视角（14.5.8）——不套人工 / 抑制 / 补位 / 迟滞 / 媒体上限，
// 资格只保留 TEMPLATE_DISABLED / BELOW_MIN_DECAYED / BELOW_MIN_USERS（绝对阈值），取前 global-top-n。
func ComputeGlobal(in GlobalInput) ScopeResult {
	p := in.Params
	res := ScopeResult{Scope: ScopeAll, Stats: ScopeStats{Skip: map[string]int{}, AutoSlots: p.GlobalTopN,
		Thresholds: Thresholds{EffMinDecayed: p.MinDecayed, EffMinUsers: p.MinUniqueUsers}}}
	prevEntered := map[int64]time.Time{}
	for _, it := range in.Prev {
		prevEntered[it.TemplateID] = it.EnteredAt
	}
	maxDecayed := 0.0
	for _, st := range in.Stats {
		maxDecayed = math.Max(maxDecayed, st.Decayed)
	}
	sc := ScoreContext{MaxDecayed: maxDecayed}
	var pool []*cand
	for id, st := range in.Stats {
		res.Stats.Candidates++
		switch {
		case !in.Templates[id].Visible:
			res.Stats.Skip[SkipDisabled]++
			continue
		case st.Decayed < p.MinDecayed:
			res.Stats.Skip[SkipBelowMinDecayed]++
			continue
		case st.UniqueUsers < p.MinUniqueUsers:
			res.Stats.Skip[SkipBelowMinUsers]++
			continue
		}
		c := &cand{id: id, st: st, media: in.Templates[id].MediaType}
		c.score, c.detail = Score(st, p, sc)
		c.eff = c.score
		pool = append(pool, c)
	}
	res.Stats.Eligible = len(pool)
	sort.Slice(pool, func(i, j int) bool { return less(pool[i], pool[j]) })
	for i, c := range pool {
		if i >= p.GlobalTopN {
			res.Stats.Skip[SkipCutBySlots]++
			if i < p.GlobalTopN+10 {
				res.Stats.NearMiss = append(res.Stats.NearMiss, NearMiss{TemplateID: c.id, Score: c.score, EffScore: c.eff, Reason: SkipCutBySlots, Uses7d: c.st.Uses7d})
			}
			continue
		}
		score := c.score
		uu := c.st.UniqueUsers
		ent := in.Now
		if t, ok := prevEntered[c.id]; ok && !t.IsZero() {
			ent = t
		}
		res.Items = append(res.Items, Item{
			Scope: ScopeAll, TemplateID: c.id, Rank: i + 1, Source: SourceAuto, Score: &score,
			Detail: &ScoreDetail{Scope: ScopeAll, Signals: c.detail, Stats: c.st, Thresholds: res.Stats.Thresholds,
				MediaCap: MediaCap{MediaType: c.media}, PickedBy: "SCORE"},
			Why: AutoWhy(c.st, p), Uses7d: c.st.Uses7d, PrevUses: c.st.PrevUses, Trend: c.st.Trend(), IsNew: c.st.IsNew(),
			UniqueUsers: &uu, EnteredAt: ent,
		})
	}
	res.Stats.Auto = len(res.Items)
	if res.Items == nil {
		res.Items = []Item{}
	}
	return res
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func hasManual(m map[int64]ManualRule, id int64) bool {
	_, ok := m[id]
	return ok
}

// tiePriority 名次限动同位时的先后：被限住下滑（clampedBy>0）0，未受限 1，被限住上升（clampedBy<0）2。
func tiePriority(clampedBy int) int {
	switch {
	case clampedBy > 0:
		return 0
	case clampedBy < 0:
		return 2
	}
	return 1
}
