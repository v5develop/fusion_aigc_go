package algo

import (
	"sort"
	"time"
)

// RefreshInput 一次整版刷新的全部输入（所有范围共享一次信号读取，14.2.1）。
type RefreshInput struct {
	Config    Config
	Scopes    []Scope
	Templates map[int64]Template
	Signals   map[int64]Signal
	Rules     []ManualRule
	Blocks    []Block
	// Prev 上一版快照按范围分组的条目
	Prev map[string][]PrevItem
	Now  time.Time
	// OnlyScopes 非空时只计算这些范围（试算用）；ALL 照常按 IncludeGlobal 决定
	OnlyScopes    map[string]bool
	IncludeGlobal bool
}

// RefreshResult 一次刷新的产物：各范围榜单（行业范围按 code 排序，ALL 在最后）。
type RefreshResult struct {
	Scopes         []ScopeResult
	CandidateCount int // 有信号的模板数（跨范围去重）
}

// ItemCount 全部范围的条目总数。
func (r RefreshResult) ItemCount() int {
	n := 0
	for _, s := range r.Scopes {
		n += len(s.Items)
	}
	return n
}

// Scope 取某范围的结果。
func (r RefreshResult) Scope(code string) (ScopeResult, bool) {
	for _, s := range r.Scopes {
		if s.Scope == code {
			return s, true
		}
	}
	return ScopeResult{}, false
}

// ComputeStatsAll 每个有信号的模板算一次统计量（参数里只有全局项影响统计量，行业覆盖只影响资格与名额）。
func ComputeStatsAll(signals map[int64]Signal, templates map[int64]Template, now time.Time, p Params) map[int64]Stats {
	out := make(map[int64]Stats, len(signals))
	for id, sig := range signals {
		st := ComputeStats(sig, templates[id].CreatedAt, now, p)
		if st.Uses7d == 0 && st.PrevUses == 0 {
			continue // 窗口外的陈旧桶不算候选
		}
		out[id] = st
	}
	return out
}

// Refresh 整版计算：逐范围独立（资格、迟滞、驻留、补位、人工合并互不影响），ALL 另算原始热度。
func Refresh(in RefreshInput) RefreshResult {
	stats := ComputeStatsAll(in.Signals, in.Templates, in.Now, in.Config.Global)
	res := RefreshResult{CandidateCount: len(stats)}
	scopes := append([]Scope(nil), in.Scopes...)
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Code < scopes[j].Code })
	for _, s := range scopes {
		if s.Code == ScopeAll || (len(in.OnlyScopes) > 0 && !in.OnlyScopes[s.Code]) {
			continue
		}
		res.Scopes = append(res.Scopes, ComputeScope(ScopeInput{
			Scope: s.Code, Members: s.Members, Templates: in.Templates, Stats: stats,
			Rules: in.Rules, Blocks: in.Blocks, Prev: in.Prev[s.Code], Params: in.Config.For(s.Code), Now: in.Now,
		}))
	}
	if in.IncludeGlobal {
		res.Scopes = append(res.Scopes, ComputeGlobal(GlobalInput{
			Templates: in.Templates, Stats: stats, Prev: in.Prev[ScopeAll], Params: in.Config.For(ScopeAll), Now: in.Now,
		}))
	}
	return res
}

// ScopeDiff 相对上一版的变化（立即刷新 / 试算的响应）。
type ScopeDiff struct {
	Enter []int64     `json:"enter"`
	Exit  []int64     `json:"exit"`
	Moved []RankMoved `json:"moved"`
}

// RankMoved 名次变化的条目。
type RankMoved struct {
	TemplateID int64 `json:"plazaTemplateId"`
	From       int   `json:"from"`
	To         int   `json:"to"`
}

// Empty 无变化。
func (d ScopeDiff) Empty() bool { return len(d.Enter) == 0 && len(d.Exit) == 0 && len(d.Moved) == 0 }

// Diff 两版同一范围条目的差异（按模板 id 升序输出，确定性）。
func Diff(prev []PrevItem, next []Item) ScopeDiff {
	before := map[int64]int{}
	for _, it := range prev {
		before[it.TemplateID] = it.Rank
	}
	after := map[int64]int{}
	for _, it := range next {
		after[it.TemplateID] = it.Rank
	}
	d := ScopeDiff{Enter: []int64{}, Exit: []int64{}, Moved: []RankMoved{}}
	for id, r := range after {
		if pr, ok := before[id]; !ok {
			d.Enter = append(d.Enter, id)
		} else if pr != r {
			d.Moved = append(d.Moved, RankMoved{TemplateID: id, From: pr, To: r})
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			d.Exit = append(d.Exit, id)
		}
	}
	sort.Slice(d.Enter, func(i, j int) bool { return d.Enter[i] < d.Enter[j] })
	sort.Slice(d.Exit, func(i, j int) bool { return d.Exit[i] < d.Exit[j] })
	sort.Slice(d.Moved, func(i, j int) bool { return d.Moved[i].TemplateID < d.Moved[j].TemplateID })
	return d
}

// ToPrev 把结果条目转成下一次刷新的「上一版」输入。
func ToPrev(items []Item) []PrevItem {
	out := make([]PrevItem, len(items))
	for i, it := range items {
		out[i] = PrevItem{TemplateID: it.TemplateID, Rank: it.Rank, Source: it.Source, EnteredAt: it.EnteredAt}
	}
	return out
}
