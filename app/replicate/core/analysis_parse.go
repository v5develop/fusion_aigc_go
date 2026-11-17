package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"aigc-local-life/app/replicate/rules/analysisfix"
)

// DimKeys 十一维（与 Java VideoCloneDims.KEYS、v4 DIMS 同一套键，DESIGN §6）。
var DimKeys = []string{"model", "garment", "accessory", "product", "motion", "expression", "line", "camera", "light", "scene", "style"}

// DimLabel 维度的用户叫法（EXPERIENCE §4 术语对照：不说「维度」，说具体项名）。
var DimLabel = map[string]string{
	"model": "人物", "garment": "服装", "accessory": "配饰", "product": "商品", "motion": "动作", "expression": "表情",
	"line": "台词", "camera": "镜头", "light": "光线", "scene": "场景", "style": "风格",
}

// AnalysisResult 规范化后的分析结果（字段 camelCase，与 Java VideoCloneAnalysisResult 同口径的子集）。
//
// 「解析模型输出 → 结构」之外，已知的常见模型错误由 rules/analysisfix 的规则表在读取时纠偏（见 analysis_fix.go）；
// Java VideoCloneAnalysisParser 的其余规则（逐套服装重叠合并、主体去重、实体归类等）尚未逐条移植。
type AnalysisResult struct {
	PromptVersion string         `json:"promptVersion"`
	DurationS     float64        `json:"durationS"`
	Width         int            `json:"width"`
	Height        int            `json:"height"`
	HasAudio      bool           `json:"hasAudio"`
	Type          string         `json:"type"`
	Purpose       string         `json:"purpose"`
	AudioKind     string         `json:"audioKind"`
	AudioNote     string         `json:"audioNote"`
	Dims          map[string]Dim `json:"dims"`
	DimOrder      []string       `json:"dimOrder"`
	Subjects      []Subject      `json:"subjects"`
	Sections      []Section      `json:"sections"`
	OnScreenText  []OnScreen     `json:"onScreenText"`
	Lines         []Sentence     `json:"lines"`
	Summary       string         `json:"summary"`
	Warnings      []string       `json:"warnings,omitempty"`
	// RulesVersion 读取时套用的纠偏规则版本（rules/analysisfix.VersionOf；落库的原始结果为空）；
	// Corrections 被纠偏的项（后台查看；用户视图里去掉）。都是读取时现算，不进分析缓存键。
	RulesVersion string                   `json:"rulesVersion,omitempty"`
	Corrections  []analysisfix.Correction `json:"corrections,omitempty"`
	// Outfits 服装套数摘要（读取时由 userAnalysis 现算；落库的原始结果为空）。见 outfits.go。
	Outfits *OutfitsView `json:"outfits,omitempty"`
}

// Dim 一项的现状。
type Dim struct {
	Present     bool         `json:"present"`
	Desc        string       `json:"desc"`
	Risk        string       `json:"risk"`
	Occurrences []Occurrence `json:"occurrences,omitempty"`
}

// Occurrence 同一项在某个时段的样子（如逐套服装）。
type Occurrence struct {
	T0      float64 `json:"t0"`
	T1      float64 `json:"t1"`
	Desc    string  `json:"desc"`
	Subject string  `json:"subject,omitempty"`
}

// Subject 出镜的人。
type Subject struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	NameEn string `json:"nameEn,omitempty"`
	Desc   string `json:"desc"`
}

// Section 分镜。
type Section struct {
	Index  int     `json:"index"`
	T0     float64 `json:"t0"`
	T1     float64 `json:"t1"`
	Desc   string  `json:"desc"`
	Action string  `json:"action,omitempty"`
	Camera string  `json:"camera,omitempty"`
}

// OnScreen 画面文字。
type OnScreen struct {
	T0   float64 `json:"t0"`
	T1   float64 `json:"t1"`
	Text string  `json:"text"`
	Kind string  `json:"kind"`
}

// visionOut 画面理解的原始输出（字段与 embed 系统提示词的输出格式一致，snake_case）。
type visionOut struct {
	Type      string `json:"type"`
	Purpose   string `json:"purpose"`
	AudioKind string `json:"audio_kind"`
	AudioNote string `json:"audio_note"`
	Dims      map[string]struct {
		Present     bool   `json:"present"`
		Desc        string `json:"desc"`
		Risk        string `json:"risk"`
		Occurrences []struct {
			T0      float64 `json:"t0"`
			T1      float64 `json:"t1"`
			Desc    string  `json:"desc"`
			Subject string  `json:"subject"`
		} `json:"occurrences"`
	} `json:"dims"`
	DimOrder []string `json:"dim_order"`
	Subjects []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		NameEn string `json:"name_en"`
		Desc   string `json:"desc"`
	} `json:"subjects"`
	Sections     []visionSection `json:"sections"`
	OnScreenText []OnScreen      `json:"on_screen_text"`
	Lines        []Sentence      `json:"lines"`
}

// visionSection 画面理解输出的一个分镜。
type visionSection struct {
	T0     float64 `json:"t0"`
	T1     float64 `json:"t1"`
	Desc   string  `json:"desc"`
	Action string  `json:"action"`
	Camera string  `json:"camera"`
}

// NormalizeAnalysis 解析画面理解的输出（可能包在 ```json 里），夹紧时间、补齐缺项、生成一句话概述；不纠偏。
// 这是落库的原始结果；纠偏在读取时套用（ResolveAnalysisWith）。
func NormalizeAnalysis(content string, st *AnalysisState) (*AnalysisResult, error) {
	return normalizeAnalysis(content, st, nil)
}

// normalizeAnalysis 同 NormalizeAnalysis；cfg 非 nil 时先按纠偏规则表修正已知的常见错误（rules/analysisfix）再夹紧，
// 被纠偏的项记进 Corrections（后台查看），规则版本记进 RulesVersion。
func normalizeAnalysis(content string, st *AnalysisState, cfg *analysisfix.Config) (*AnalysisResult, error) {
	raw := strings.TrimSpace(content)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var in visionOut
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, fmt.Errorf("画面理解输出不是 JSON: %w", err)
	}
	if len(in.Dims) == 0 && len(in.Sections) == 0 {
		return nil, errors.New("画面理解输出缺少 dims 与 sections")
	}
	d := 0.0
	r := &AnalysisResult{PromptVersion: AnalysisPromptVersion, Type: oneOf(in.Type, "other", "outfit", "product", "talk", "scene", "dance", "food", "other"),
		Purpose: oneOf(in.Purpose, "mixed", "sell", "inspire", "tutorial", "story", "portfolio", "mixed"), AudioNote: trimRunes(in.AudioNote, 60)}
	if p := st.Probe; p != nil {
		d, r.DurationS, r.Width, r.Height, r.HasAudio = p.DurationS, p.DurationS, p.Width, p.Height, p.HasAudio
	}
	r.AudioKind = oneOf(in.AudioKind, "silent", "speech", "music", "mixed", "silent")
	if !r.HasAudio {
		r.AudioKind = "silent"
	}
	// 纠偏（夹紧之前：时长类规则要看到模型给的原始时间）
	fix := applyAnalysisRules(&in, st, r, cfg)
	cl := func(t float64) float64 {
		if math.IsNaN(t) || t < 0 {
			return 0
		}
		if d > 0 && t > d {
			return d
		}
		return math.Round(t*10) / 10
	}
	r.Dims = map[string]Dim{}
	for _, k := range DimKeys {
		x, ok := in.Dims[k]
		dim := Dim{Present: ok && x.Present, Desc: trimRunes(x.Desc, 80), Risk: oneOf(x.Risk, "mid", "low", "mid", "high")}
		if k == "line" {
			dim.Present, dim.Desc = fix.linePresent, trimRunes(fix.lineDesc, 80)
		}
		if !dim.Present {
			dim.Desc = ""
		}
		for _, o := range x.Occurrences {
			if t0, t1 := cl(o.T0), cl(o.T1); t1 > t0 && dim.Present {
				dim.Occurrences = append(dim.Occurrences, Occurrence{T0: t0, T1: t1, Desc: trimRunes(o.Desc, 80), Subject: o.Subject})
			}
		}
		r.Dims[k] = dim
	}
	// 有转写时「台词」以转写为准（转写时间原样保留）
	if fix.linesFromTranscript {
		r.Lines = fix.lines
	} else {
		for _, l := range fix.lines {
			if t0, t1 := cl(l.T0), cl(l.T1); t1 > t0 && strings.TrimSpace(l.Text) != "" {
				r.Lines = append(r.Lines, Sentence{T0: t0, T1: t1, Text: l.Text})
			}
		}
	}
	seen := map[string]bool{}
	for _, k := range in.DimOrder {
		if _, ok := DimLabel[k]; ok && !seen[k] {
			r.DimOrder, seen[k] = append(r.DimOrder, k), true
		}
	}
	for _, k := range DimKeys {
		if !seen[k] {
			r.DimOrder = append(r.DimOrder, k)
		}
	}
	for _, s := range in.Subjects {
		if s.ID != "" {
			r.Subjects = append(r.Subjects, Subject{ID: s.ID, Name: trimRunes(s.Name, 40), NameEn: s.NameEn, Desc: trimRunes(s.Desc, 80)})
		}
	}
	// 分镜：夹紧、排序、去重叠，首尾补齐到 [0, D]
	for _, s := range in.Sections {
		if t0, t1 := cl(s.T0), cl(s.T1); t1 > t0 {
			r.Sections = append(r.Sections, Section{T0: t0, T1: t1, Desc: trimRunes(s.Desc, 80), Action: trimRunes(s.Action, 60), Camera: trimRunes(s.Camera, 40)})
		}
	}
	sort.SliceStable(r.Sections, func(i, j int) bool { return r.Sections[i].T0 < r.Sections[j].T0 })
	var secs []Section
	for _, s := range r.Sections {
		if n := len(secs); n > 0 && s.T0 < secs[n-1].T1 {
			s.T0 = secs[n-1].T1
		}
		if s.T1 > s.T0 {
			secs = append(secs, s)
		}
	}
	if len(secs) == 0 && d > 0 {
		secs = []Section{{T0: 0, T1: d, Desc: r.Dims["scene"].Desc}}
		r.Warnings = append(r.Warnings, "没能完全看懂画面，可以重新分析，或直接写描述")
	}
	if len(secs) > 0 {
		secs[0].T0 = 0
		if d > 0 {
			secs[len(secs)-1].T1 = d
		}
	}
	for i := range secs {
		secs[i].Index = i + 1
	}
	r.Sections = secs
	for _, o := range in.OnScreenText {
		if t0, t1 := cl(o.T0), cl(o.T1); t1 > t0 && strings.TrimSpace(o.Text) != "" {
			r.OnScreenText = append(r.OnScreenText, OnScreen{T0: t0, T1: t1, Text: o.Text, Kind: oneOf(o.Kind, "other", "subtitle", "watermark", "sticker", "price", "title", "other")})
		}
	}
	r.Warnings = append(r.Warnings, filterUserWarnings(st.Warnings)...)
	r.Summary = summarize(r)
	return r, nil
}

// summarize 概述模板：看懂了：{N} 个镜头，{主体概述}；{声音概述}；{文字概述}（EXPERIENCE §4.3）。
func summarize(r *AnalysisResult) string {
	var parts []string
	for i, s := range r.Sections {
		if i >= 3 {
			break
		}
		if d := strings.TrimSpace(firstNonEmpty(s.Action, s.Desc)); d != "" {
			parts = append(parts, trimRunes(d, 12))
		}
	}
	subject := strings.Join(parts, "、")
	if len(r.Sections) > 3 {
		subject += "等"
	}
	var voice string
	switch r.AudioKind {
	case "speech":
		voice = "有人声讲解"
	case "music":
		voice = "只有背景音乐"
	case "mixed":
		voice = "有人声和背景音乐"
	default:
		voice = "没有声音"
	}
	text := "画面上没有字幕"
	if len(r.OnScreenText) > 0 {
		text = "画面上有文字"
	}
	s := fmt.Sprintf("看懂了：%d 个镜头", len(r.Sections))
	if subject != "" {
		s += "，" + subject
	}
	return s + "；" + voice + "；" + text
}

func oneOf(v, def string, allowed ...string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

func trimRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
