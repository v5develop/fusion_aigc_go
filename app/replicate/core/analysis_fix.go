package core

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/analysisfix"
)

// 本文件：分析结果纠偏（rules/analysisfix 规则表）与 AnalysisResult 的互转。
//
// 应用时机：读取时套用，不在分析时固化。分析完成时只落「未纠偏」的规范化结果（afv_replicate_analysis.result）
// 与画面理解原文（state.visionContent）；返回用户端分析 / 生成方案 / 后台详情时，用当前规则配置对原文重新规范化
// （纠偏在夹紧时间之前，时长类规则要看到模型给的原始时间）。所以改规则 / 改配置不让已有分析失效、不重新调用网关，
// 分析缓存键（source_key）也不含规则版本。

// RulesVersion 当前运行参数下的纠偏规则版本（读取套用时写进结果的 rulesVersion）。
func (s *Service) RulesVersion() string { return analysisfix.VersionOf(s.cfg.AnalysisRules) }

// ResolveAnalysis 按当前运行参数的纠偏规则读出分析结果（见 ResolveAnalysisWith）。
func (s *Service) ResolveAnalysis(a *model.Analysis) (*AnalysisResult, error) {
	return ResolveAnalysisWith(a, s.cfg.AnalysisRules)
}

// ResolveAnalysisWith 读取时套用纠偏：state 里有画面理解原文 → 按 cfg 重新规范化（带 corrections / rulesVersion）；
// 没有原文（直接写入结果的系统数据、测试数据）或原文重放失败 → 原样返回落库结果（不纠偏）。
// 结果为空或无法解析返回 error。
func ResolveAnalysisWith(a *model.Analysis, cfg analysisfix.Config) (*AnalysisResult, error) {
	if a == nil {
		return nil, errNoAnalysisResult
	}
	if len(a.State) > 0 {
		var st AnalysisState
		if json.Unmarshal(a.State, &st) == nil && strings.TrimSpace(st.VisionContent) != "" {
			c := cfg.Normalize()
			if r, err := normalizeAnalysis(st.VisionContent, &st, &c); err == nil {
				return r, nil
			}
		}
	}
	if len(a.Result) == 0 {
		return nil, errNoAnalysisResult
	}
	var r AnalysisResult
	if err := json.Unmarshal(a.Result, &r); err != nil {
		return nil, err
	}
	if len(a.State) > 0 {
		var st AnalysisState
		if json.Unmarshal(a.State, &st) == nil {
			r.Motion = st.Motion.toSegment()
		}
	}
	return &r, nil
}

var errNoAnalysisResult = errors.New("分析结果为空")

// fixedAnalysis 纠偏后、NormalizeAnalysis 继续规范化要用的值。
type fixedAnalysis struct {
	lines               []Sentence
	linesFromTranscript bool
	linePresent         bool
	lineDesc            string
}

// applyAnalysisRules 把画面理解输出 + 转写 + 关键帧转成规则的 Doc，按表纠偏后写回 in 与 r（AudioKind / RulesVersion / Corrections）。
// cfg 为 nil = 不纠偏（只做 Doc 互转，落库的原始结果走这条），rulesVersion / corrections 留空。
// 带时间的「其它项」按 维度顺序的 occurrences → 画面文字 的顺序展开、原序写回。
func applyAnalysisRules(in *visionOut, st *AnalysisState, r *AnalysisResult, cfg *analysisfix.Config) fixedAnalysis {
	doc := analysisfix.Doc{DurationS: r.DurationS, HasAudio: r.HasAudio, AudioKind: r.AudioKind}
	for _, f := range st.Frames {
		doc.Frames = append(doc.Frames, f.T)
	}
	sort.Float64s(doc.Frames)
	lineDim := in.Dims["line"]
	doc.LinePresent, doc.LineDesc = lineDim.Present, strings.TrimSpace(lineDim.Desc)
	if t := st.Transcript; t != nil {
		doc.TranscriptRan = true
		if len(t.Sentences) > 0 {
			doc.LinesFromTranscript, doc.LinePresent = true, true
			if doc.LineDesc == "" {
				doc.LineDesc = strings.TrimSpace(t.Text)
			}
			for _, x := range t.Sentences {
				doc.Lines = append(doc.Lines, analysisfix.Line{T0: x.T0, T1: x.T1, Text: x.Text})
			}
		}
	}
	if !doc.LinesFromTranscript {
		for _, l := range in.Lines {
			doc.Lines = append(doc.Lines, analysisfix.Line{T0: l.T0, T1: l.T1, Text: l.Text})
		}
	}
	for _, sc := range in.Sections {
		doc.Sections = append(doc.Sections, analysisfix.Section{T0: sc.T0, T1: sc.T1, Desc: sc.Desc, Action: sc.Action, Camera: sc.Camera})
	}
	for _, k := range DimKeys {
		x, ok := in.Dims[k]
		if !ok {
			continue
		}
		for _, o := range x.Occurrences {
			doc.Others = append(doc.Others, analysisfix.Span{T0: o.T0, T1: o.T1})
		}
	}
	for _, o := range in.OnScreenText {
		doc.Others = append(doc.Others, analysisfix.Span{T0: o.T0, T1: o.T1})
	}

	if cfg != nil {
		r.Corrections = analysisfix.Apply(&doc, *cfg)
		r.RulesVersion = analysisfix.VersionOf(*cfg)
	}

	// 写回
	in.Sections = in.Sections[:0]
	for _, sc := range doc.Sections {
		in.Sections = append(in.Sections, visionSection{T0: sc.T0, T1: sc.T1, Desc: sc.Desc, Action: sc.Action, Camera: sc.Camera})
	}
	i := 0
	for _, k := range DimKeys {
		x, ok := in.Dims[k]
		if !ok {
			continue
		}
		for j := range x.Occurrences {
			x.Occurrences[j].T0, x.Occurrences[j].T1 = doc.Others[i].T0, doc.Others[i].T1
			i++
		}
		in.Dims[k] = x
	}
	for j := range in.OnScreenText {
		in.OnScreenText[j].T0, in.OnScreenText[j].T1 = doc.Others[i].T0, doc.Others[i].T1
		i++
	}
	r.AudioKind = doc.AudioKind
	out := fixedAnalysis{linesFromTranscript: doc.LinesFromTranscript, linePresent: doc.LinePresent, lineDesc: doc.LineDesc}
	for _, l := range doc.Lines {
		out.lines = append(out.lines, Sentence{T0: l.T0, T1: l.T1, Text: l.Text})
	}
	return out
}

// userAnalysis 用户视图里的分析结果：去掉纠偏明细（只给后台看）。
func userAnalysis(r *AnalysisResult) *AnalysisResult {
	if r == nil {
		return r
	}
	ov := outfitsView(r)
	if len(r.Corrections) == 0 && ov == nil {
		return r
	}
	c := *r
	c.Corrections = nil
	c.Outfits = ov // 逐套服装摘要（CONTRACT §O1），读取时现算，不进落库的原始结果
	return &c
}
