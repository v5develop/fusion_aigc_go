package core

import (
	"fmt"
	"sort"
	"strings"

	"aigc-local-life/app/replicate/rules/segment"
)

// ---------------------------------------------------------------------------------------------
// 逐套换装（CONTRACT §O1）：分析把「服装」按时间拆成多套（dims.garment.occurrences，每套一个时间区间），
// 方案可以只换其中几套（DimEdit.Occurrences），没列出的套保持原样。
//
//   - 套号（index）从 1 起，按开始时间升序（outfitsOf 统一排序，分析摘要、方案文案、提示词三处同一口径）。
//   - 只在这一项有 ≥2 次出现时才允许逐套标注；不标注时行为与旧版完全一致（JSON 与 plan_hash 不变）。
//   - 标注了逐套后，维度级 Refs 被忽略并清空（「换哪几套、各换成哪张」只由逐套说了算），避免两处口径打架。
// ---------------------------------------------------------------------------------------------

// OccurrenceEdit 某一项的第 Index 套换成 Refs 指定的参考图（Refs 是 PlanReq.RefImages 的图号，从 1 起）。
type OccurrenceEdit struct {
	Index int   `json:"index"`
	Refs  []int `json:"refs"`
}

// OutfitBrief 分析里识别出的一套造型（给智能体 / 卡片用的摘要）。
type OutfitBrief struct {
	Index int     `json:"index"`
	T0    float64 `json:"t0"`
	T1    float64 `json:"t1"`
	Desc  string  `json:"desc"`
}

// OutfitsView AnalysisView.result.outfits：套数与每套的时间区间、简短描述。
type OutfitsView struct {
	Count int           `json:"count"`
	Items []OutfitBrief `json:"items"`
}

// outfitsOf 某一项的各次出现，按开始时间升序（稳定）；下标 + 1 即套号。
func outfitsOf(d Dim) []Occurrence {
	out := append([]Occurrence(nil), d.Occurrences...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].T0 < out[j].T0 })
	return out
}

// outfitsView 分析结果里的服装套数摘要；没有服装 / 没有逐套信息返回 nil。
func outfitsView(r *AnalysisResult) *OutfitsView {
	if r == nil {
		return nil
	}
	g, ok := r.Dims["garment"]
	if !ok || !g.Present {
		return nil
	}
	occ := outfitsOf(g)
	if len(occ) == 0 {
		return nil
	}
	v := &OutfitsView{Count: len(occ)}
	for i, o := range occ {
		v.Items = append(v.Items, OutfitBrief{Index: i + 1, T0: o.T0, T1: o.T1, Desc: trimRunes(o.Desc, 40)})
	}
	return v
}

// editRefs 一项用到的全部图号（去重、升序）：维度级 Refs ∪ 各套的 Refs。
func editRefs(e DimEdit) []int {
	seen := map[int]bool{}
	var out []int
	add := func(rs []int) {
		for _, r := range rs {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	add(e.Refs)
	for _, o := range e.Occurrences {
		add(o.Refs)
	}
	sort.Ints(out)
	return out
}

// hasReplaceRefs 这一项是否是「换成我的图」且确有图（批量、预览用）。
func hasReplaceRefs(e DimEdit) bool { return e.Mode == ModeReplace && len(editRefs(e)) > 0 }

// occRefsOf 第 index 套被指定的图号；没指定返回 nil（保持原样）。
func occRefsOf(e DimEdit, index int) []int {
	for _, o := range e.Occurrences {
		if o.Index == index {
			return o.Refs
		}
	}
	return nil
}

// occText 把逐套换装写成人读的一句话：「第 1 套 → 图 2；第 3 套 → 图 3」。
func occText(e DimEdit) string {
	var parts []string
	for _, o := range e.Occurrences {
		parts = append(parts, fmt.Sprintf("第 %d 套 → %s", o.Index, imgNames(o.Refs)))
	}
	return strings.Join(parts, "；")
}

func imgNames(refs []int) string {
	ns := make([]string, len(refs))
	for i, r := range refs {
		ns[i] = fmt.Sprintf("图 %d", r)
	}
	return strings.Join(ns, "、")
}

// outfitSlice 某套在某一段里的样子（时间相对本段起点）。
type outfitSlice struct {
	N        int
	From, To float64
	Desc     string
	Refs     []int // 空 = 这套保持原样
}

// outfitsInSegment 与本段生成窗口 [GenT0, GenT1) 有时间重叠的各套（按套号升序），带这套是否被换。
func outfitsInSegment(dim Dim, e DimEdit, seg segment.Segment) []outfitSlice {
	var out []outfitSlice
	for i, o := range outfitsOf(dim) {
		if o.T1 <= seg.GenT0 || o.T0 >= seg.GenT1 {
			continue
		}
		out = append(out, outfitSlice{N: i + 1, From: maxF(o.T0, seg.GenT0) - seg.GenT0, To: minF(o.T1, seg.GenT1) - seg.GenT0,
			Desc: o.Desc, Refs: occRefsOf(e, i+1)})
	}
	return out
}
