package core

import (
	"embed"
	"fmt"
	"strings"
	"text/template"

	"aigc-local-life/app/replicate/rules/segment"
)

// 模型方言（DESIGN §3 / §5.11 / §7）：结构在代码（本文件组装视图），措辞与约束在内嵌模板（prompts/dialects/*.tmpl）。
//
// 移植来源：Java VideoClonePrompt（seedance 线上写法 @视频1 / @图片n，零行为变化的标签）与 v4 studio/engine.py 的
// 参考生成渲染器（render_seedance25_ref / render_seedance20_ref / render_minimax_ref / _wan3_body）。
// 不移植：编辑式（video_editing）渲染、<Audio 1> 引用（我们不向网关传参考音频）。
// 逐套换装（CONTRACT §O1）：方案标注了 DimEdit.Occurrences 时，每段按时间重叠找到出现的各套，逐套写「第 N 套换成图 X」（occText 见 outfitsText）。
// 网关不改写提示词里的素材标签（已核实 ai-gateway adapters 无标签转换），所以标签必须是各家原生写法。

//go:embed prompts/dialects/*.tmpl
var dialectFS embed.FS

// Dialect 一家模型的段提示词方言。
type Dialect struct {
	Code     string // seedance-2.5 / seedance-2 / minimax-h3 / wan-3.0 / base
	Version  string // 进 plan_hash；模板一改要升版本
	VideoTag string
	ImageFmt string // 参考图标签（%d = 本地图号）
	MaxChars int    // 网关 prompt_max 5000，留余量
	// WriterStyle 写手按方言写作的要点（写手系统提示词里引用）。
	WriterStyle string
	// RiskTerms 写手产出里出现即判不合格（v4 writer._check 风控词）。
	RiskTerms []string
	tmpl      *template.Template
}

var riskTerms = []string{"紧身", "贴身", "曲线", "肌肤", "裸露", "身材", "丝袜", "黑丝", "渔网"}

func mustDialect(d Dialect) *Dialect {
	b, err := dialectFS.ReadFile("prompts/dialects/" + d.Code + ".tmpl")
	if err != nil {
		panic(err)
	}
	d.tmpl = template.Must(template.New(d.Code).Funcs(template.FuncMap{"inc": func(i int) int { return i + 1 }}).Parse(string(b)))
	if d.MaxChars == 0 {
		d.MaxChars = 4800
	}
	d.RiskTerms = riskTerms
	return &d
}

var (
	dialectBase = mustDialect(Dialect{Code: "base", Version: "base-1", VideoTag: "@视频1", ImageFmt: "@图片%d",
		WriterStyle: "中文，参考式改写：@视频1 是原片该段，参考图按 @图片n 引用。"})
	dialects = map[string]*Dialect{
		"seedance-2.5": mustDialect(Dialect{Code: "seedance-2.5", Version: "seedance-2.5-1", VideoTag: "@视频1", ImageFmt: "@图片%d",
			WriterStyle: "中文自然段 + 整数秒分镜；先总述「以 @视频1 的动作、走位、运镜和剪辑节奏为参考」，再逐项写替换（标签 + 只参考哪些属性），时间锚点写整数秒；强调同一主体全片外观一致；结尾写声音与负面约束。"}),
		"seedance-2": mustDialect(Dialect{Code: "seedance-2", Version: "seedance-2-1", VideoTag: "@视频1", ImageFmt: "@图片%d",
			WriterStyle: "中文；先用「将 @图片n 中的X定义为<主体k>」逐个定义主体，再写「参考 @视频1 中的动作、走位、运镜和剪辑节奏，顺序保持一致，生成含上述主体的视频」；镜头顺序用短句，避免长段落。"}),
		"minimax-h3": mustDialect(Dialect{Code: "minimax-h3", Version: "minimax-h3-1", VideoTag: "<Video 1>", ImageFmt: "<Picture %d>", MaxChars: 4000,
			WriterStyle: "English, sectioned: subject_definitions / summary / retention_analysis / detailed_description / overall_soundscape / negative. Pictures are <Picture n>, the source clip is <Video 1>. Define each replaced subject as <Subject k> and state which attributes are referenced."}),
		"wan-3.0": mustDialect(Dialect{Code: "wan-3.0", Version: "wan-3.0-1", VideoTag: "视频1", ImageFmt: "图%d", MaxChars: 2400,
			WriterStyle: "中文万相公式：总体描述 + 参考素材引用（视频1 / 图n）+ 分镜N（起-止秒）逐镜写机位、动作与替换 + 风格情绪 + 负向清单；每个分镜各写一遍场景。"}),
	}
)

// DialectFor 按模型组选方言：prime / fast / mini 复用家族方言，未知模型回落基础模板。
func DialectFor(group string) *Dialect {
	g := strings.ToLower(strings.TrimSpace(group))
	switch {
	case strings.HasPrefix(g, "vid.seedance-2.5"):
		return dialects["seedance-2.5"]
	case strings.HasPrefix(g, "vid.seedance-2"):
		return dialects["seedance-2"]
	case strings.HasPrefix(g, "vid.minimax-h3"):
		return dialects["minimax-h3"]
	case strings.HasPrefix(g, "vid.wan-3"):
		return dialects["wan-3.0"]
	}
	return dialectBase
}

type changeView struct {
	Key, Label, Noun, Attrs, Current, Tags, Note, Hint string
	// 逐套换装（CONTRACT §O1，仅标注了 Occurrences 的项有值）：OccText 是本段逐套写法（各方言措辞自带）；
	// SubjNo 本项第一个主体编号（seedance-2 / minimax 的「主体 k」连续编号，逐套时一项占多个）；
	// ShotText 分镜尾注里这一项的写法；Retain 保持一致性的主体描述（minimax）。
	OccText, ShotText, Retain string
	SubjNo                    int
}

type keepView struct{ Label, Desc string }

type shotView struct {
	From, To     float64
	Text, Camera string
}

type promptView struct {
	Multi                       bool
	Index, Total, Duration      int
	GenT0, GenT1                float64
	Video                       string
	Replace, Describe           []changeView
	Keep                        []keepView
	KeepNames, ChangedLabels    string
	Shots                       []shotView
	Audio                       string
	LineChanged                 bool
	Camera, Scene, Light, Style string
}

var dimNounEn = map[string]string{"model": "person", "garment": "outfit", "accessory": "accessory", "product": "product", "scene": "scene",
	"motion": "motion", "expression": "expression", "line": "lines", "camera": "camera work", "light": "lighting", "style": "style"}

var dimAttrsEn = map[string]string{"garment": "silhouette, collar, sleeves, hem, fabric, color and patterns",
	"product": "shape, color, material, logo placement and apparent scale", "accessory": "style, material, color and apparent size",
	"model": "facial features, hairstyle and body shape", "scene": "layout, decoration, signage and lighting of the space"}

// refHint 只参考图里的哪些属性（v4 REF_HINT 的中文口径）。
var refHint = map[string]string{"model": "只参考脸、发型与体态，服装沿用原片", "garment": "只参考服装的形制、颜色与纹样，不参考图中人物与姿势",
	"accessory": "只参考配饰的款式、材质与颜色", "product": "只参考商品的外形、颜色、材质与 Logo 位置，大小按画面比例",
	"scene": "只参考场景的布局、装修与门头"}

func buildPromptView(d *Dialect, an *AnalysisResult, ann Annotations, refCount int, seg segment.Segment, total int) promptView {
	audio := ann.Audio
	if audio == AudioRevoice {
		audio = AudioKeep // 换声音：模型不生成新的声音（成片的声音由配音替换），提示词与保留原声同口径
	}
	v := promptView{Multi: total > 1, Index: seg.Index, Total: total, Duration: seg.GenDuration, GenT0: seg.GenT0, GenT1: seg.GenT1,
		Video: d.VideoTag, Audio: audio}
	var keepNames, changed []string
	subjNo := 1
	for _, k := range an.DimOrder {
		dim := an.Dims[k]
		e := ann.Dims[k]
		label := DimLabel[k]
		switch e.Mode {
		case ModeReplace:
			tags := make([]string, 0, len(e.Refs))
			for _, r := range e.Refs {
				if r >= 1 && r <= refCount {
					tags = append(tags, fmt.Sprintf(d.ImageFmt, r))
				}
			}
			cv := changeView{Key: k, Label: label, Noun: dimNounEn[k], Attrs: orText(dimAttrsEn[k], "appearance"),
				Current: orText(dim.Desc, label), Tags: strings.Join(tags, "、"), Note: strings.TrimSpace(e.Note), Hint: orText(refHint[k], "按参考图重建"),
				SubjNo: subjNo}
			if len(e.Occurrences) > 0 {
				// 逐套换装：本段没有任何被换的套（只露出保持原样的套）就当「不改」处理
				slices := outfitsInSegment(dim, e, seg)
				replaced := 0
				for _, o := range slices {
					if len(o.Refs) > 0 {
						replaced++
					}
				}
				if replaced == 0 {
					if dim.Present && dim.Desc != "" {
						v.Keep = append(v.Keep, keepView{Label: label, Desc: dim.Desc})
						keepNames = append(keepNames, label)
					}
					continue
				}
				fillOutfits(d, &cv, slices, refCount)
				subjNo += replaced
			} else {
				cv.ShotText = fmt.Sprintf("%s为 %s 中的内容", label, cv.Tags)
				cv.Retain = fmt.Sprintf("<Subject %d>: identity must stay consistent in every shot.", subjNo)
				subjNo++
			}
			v.Replace = append(v.Replace, cv)
			changed = append(changed, label)
			if k == "line" {
				v.LineChanged = true
			}
		case ModeDescribe:
			v.Describe = append(v.Describe, changeView{Key: k, Label: label, Note: strings.TrimSpace(e.Note)})
			changed = append(changed, label)
			if k == "line" {
				v.LineChanged = true
			}
		default:
			if dim.Present && dim.Desc != "" {
				v.Keep = append(v.Keep, keepView{Label: label, Desc: dim.Desc})
				keepNames = append(keepNames, label)
				switch k {
				case "camera":
					v.Camera = dim.Desc
				case "scene":
					v.Scene = dim.Desc
				case "light":
					v.Light = dim.Desc
				case "style":
					v.Style = dim.Desc
				}
			}
		}
	}
	v.KeepNames, v.ChangedLabels = strings.Join(keepNames, "、"), strings.Join(changed, "、")
	for _, s := range an.Sections {
		if s.T1 > seg.GenT0 && s.T0 < seg.GenT1 {
			v.Shots = append(v.Shots, shotView{From: maxF(s.T0, seg.GenT0) - seg.GenT0, To: minF(s.T1, seg.GenT1) - seg.GenT0,
				Text: orText(s.Action, s.Desc), Camera: s.Camera})
		}
	}
	return v
}

// RenderDialect 用方言模板渲染一段提示词；超长时先砍分镜（Java VideoClonePrompt 同口径），仍超长按字截断。
func RenderDialect(d *Dialect, an *AnalysisResult, ann Annotations, refCount int, seg segment.Segment, total int) string {
	v := buildPromptView(d, an, ann, refCount, seg, total)
	for {
		var b strings.Builder
		if err := d.tmpl.Execute(&b, v); err != nil {
			return renderBase(an, ann, refCount, seg, total)
		}
		out := strings.TrimSpace(b.String())
		if len([]rune(out)) <= d.MaxChars {
			return out
		}
		if len(v.Shots) == 0 {
			return string([]rune(out)[:d.MaxChars])
		}
		v.Shots = v.Shots[:len(v.Shots)-1]
	}
}

func renderBase(an *AnalysisResult, ann Annotations, refCount int, seg segment.Segment, total int) string {
	var b strings.Builder
	_ = dialectBase.tmpl.Execute(&b, buildPromptView(dialectBase, an, ann, refCount, seg, total))
	return strings.TrimSpace(b.String())
}

// fillOutfits 逐套换装：按本段露出的各套写逐套指令（被换的套「换成图 X 的样子」，没被换的套「保持原样」）。
// Tags 取本段用到的图（去重，供「参考素材」类汇总行用）。
func fillOutfits(d *Dialect, cv *changeView, slices []outfitSlice, refCount int) {
	tagOf := func(refs []int) string {
		var ts []string
		for _, r := range refs {
			if r >= 1 && r <= refCount {
				ts = append(ts, fmt.Sprintf(d.ImageFmt, r))
			}
		}
		return strings.Join(ts, "、")
	}
	var all []int
	for _, o := range slices {
		all = append(all, o.Refs...)
	}
	cv.Tags = strings.Join(uniqTags(all, tagOf), "、")
	cv.ShotText = "各套" + cv.Label + "按上面逐套指定的图处理，没指定的套保持原样"
	en := d.Code == "minimax-h3"
	total := len(slices)
	var parts, keeps, retain []string
	subj := cv.SubjNo
	for _, o := range slices {
		span := fmt.Sprintf("%.1f–%.1f", o.From, o.To)
		if len(o.Refs) == 0 {
			if en {
				keeps = append(keeps, fmt.Sprintf("%s %d", cv.Noun, o.N))
			} else {
				keeps = append(keeps, fmt.Sprintf("第 %d 套", o.N))
			}
			continue
		}
		tags := tagOf(o.Refs)
		switch {
		case en:
			parts = append(parts, fmt.Sprintf("<Subject %d> is the new %s in %s; it replaces %s %d (%ss in this clip, originally: %s). Only the %s are referenced; background, pose and watermark in the picture are ignored.",
				subj, cv.Noun, tags, cv.Noun, o.N, span, orText(o.Desc, cv.Noun), cv.Attrs))
			retain = append(retain, fmt.Sprintf("<Subject %d>: identity must stay consistent in every shot.", subj))
		case d.Code == "seedance-2":
			parts = append(parts, fmt.Sprintf("将 %s 中的%s定义为<主体%d>，用于替换原片第 %d 套%s（本段第 %s 秒，原片是「%s」），%s",
				tags, cv.Label, subj, o.N, cv.Label, span, orText(o.Desc, cv.Label), cv.Hint))
		default:
			parts = append(parts, fmt.Sprintf("第 %d 套%s（本段第 %s 秒，原片是「%s」）换成 %s 的样子，%s", o.N, cv.Label, span, orText(o.Desc, cv.Label), tags, cv.Hint))
		}
		subj++
	}
	switch {
	case en:
		cv.OccText = strings.Join(parts, "\n")
		if len(keeps) > 0 {
			cv.OccText += fmt.Sprintf("\nKeep %s exactly as in the source; do not replace.", strings.Join(keeps, ", "))
		}
		cv.Retain = strings.Join(retain, "\n")
	case d.Code == "seedance-2":
		cv.OccText = strings.Join(parts, "。\n") + "。"
		if len(keeps) > 0 {
			cv.OccText += fmt.Sprintf("\n%s%s保持原样，不要替换。", strings.Join(keeps, "、"), cv.Label)
		}
	default:
		head := fmt.Sprintf("%s在本段出现 %d 套，逐套处理：", cv.Label, total)
		cv.OccText = head + strings.Join(parts, "；")
		if len(keeps) > 0 {
			cv.OccText += fmt.Sprintf("；%s%s保持原样，不要替换", strings.Join(keeps, "、"), cv.Label)
		}
		if d.Code != "wan-3.0" { // 万相模板自己收句号
			cv.OccText += "。"
		}
	}
	if n := strings.TrimSpace(cv.Note); n != "" && !en {
		cv.OccText += "补充要求：" + n + "。"
	}
}

func uniqTags(refs []int, tagOf func([]int) string) []string {
	seen := map[int]bool{}
	var out []string
	for _, r := range refs {
		if seen[r] {
			continue
		}
		seen[r] = true
		if t := tagOf([]int{r}); t != "" {
			out = append(out, t)
		}
	}
	return out
}
