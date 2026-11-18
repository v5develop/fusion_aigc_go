package core

import (
	"fmt"
	"sort"
	"strings"

	"aigc-local-life/app/replicate/rules/segment"
)

// ---------------------------------------------------------------------------------------------
// 多人点名（CONTRACT §X）：分析识别出多个人物（AnalysisResult.Subjects），方案可以只换其中几个人
// （DimEdit.Subjects，对 model 换人 / garment 换装生效），没列出的人保持原样。
//
//   - 只在分析有 ≥2 人时才允许按人指定；不指定时行为与旧版完全一致（JSON 与 plan_hash 不变）。
//   - 标注了按人之后，维度级 Refs 被忽略并清空（换谁、换成哪张只由按人说了算）；mode 为 keep 的条目等于没列，规范化时丢掉。
//   - 与逐套换装（Occurrences）并存时，逐套只作用于该维度里「未按人指定」的部分：
//     同一个人既按人又按套指定（套的 subject 是被点名的人）→ SUBJECT_OCCURRENCE_CONFLICT。
//   - 写手 / 方言按段处理：只写「本段出现」的人（分析给了此人出镜时段才判断，没给 = 每段都在）；
//     本段一个被点名的人都没出现，这一项按「不改」。
// ---------------------------------------------------------------------------------------------

// SubjectEdit 某一项里对某个人物的改法。Refs 是 PlanReq.RefImages 的图号（从 1 起）。
type SubjectEdit struct {
	ID   string `json:"id"`
	Refs []int  `json:"refs,omitempty"`
	// Mode replace / describe；规范化后总是显式写出（缺省 = 该项的 mode）。
	Mode string `json:"mode,omitempty"`
	Note string `json:"note,omitempty"`
}

// subjectsSupported 支持按人点名的维度：换人、换装。
func subjectsSupported(key string) bool { return key == "model" || key == "garment" }

// normalizeSubjects 视觉理解输出的人物 → 规范化人物表：丢空 id / 重复 id；name 补默认并保证互不相同
// （用户靠它区分谁是谁）；出镜时段夹紧、丢无效、按开始时间升序。
func normalizeSubjects(in []visionSubject, cl func(float64) float64) []Subject {
	var out []Subject
	ids, names := map[string]bool{}, map[string]int{}
	for _, s := range in {
		id := strings.TrimSpace(s.ID)
		if id == "" || ids[id] {
			continue
		}
		ids[id] = true
		name := trimRunes(s.Name, 40)
		if name == "" {
			name = fmt.Sprintf("人物%d", len(out)+1)
		}
		names[name]++
		if n := names[name]; n > 1 {
			name = fmt.Sprintf("%s（%d）", trimRunes(name, 36), n)
		}
		sub := Subject{ID: id, Name: name, NameEn: s.NameEn, Desc: trimRunes(s.Desc, 80)}
		for _, sp := range s.Segments {
			if t0, t1 := cl(sp.T0), cl(sp.T1); t1 > t0 {
				sub.Segments = append(sub.Segments, TimeSpan{T0: t0, T1: t1})
			}
		}
		sort.SliceStable(sub.Segments, func(i, j int) bool { return sub.Segments[i].T0 < sub.Segments[j].T0 })
		out = append(out, sub)
	}
	return out
}

// subjectsView 用户视图里的人物：id、name、desc（≤20 字）；没有人物返回 nil。
func subjectsView(r *AnalysisResult) []Subject {
	if r == nil || len(r.Subjects) == 0 {
		return nil
	}
	out := make([]Subject, len(r.Subjects))
	for i, s := range r.Subjects {
		s.Desc = trimRunes(s.Desc, 20)
		out[i] = s
	}
	return out
}

// subjectByID 按 id 找分析里的人物。
func subjectByID(an *AnalysisResult, id string) (Subject, bool) {
	for _, s := range an.Subjects {
		if s.ID == id {
			return s, true
		}
	}
	return Subject{}, false
}

// subjectEditOf 某个人物的改法；没点名返回 nil（保持原样）。
func subjectEditOf(e DimEdit, id string) *SubjectEdit {
	for i := range e.Subjects {
		if e.Subjects[i].ID == id {
			return &e.Subjects[i]
		}
	}
	return nil
}

// subjectMode 一个人物改法的生效 mode：自己没写就用这一项的 mode。
func subjectMode(e DimEdit, se SubjectEdit) string {
	if se.Mode != "" {
		return se.Mode
	}
	return e.Mode
}

// subjectRefs 一项里按人指定的全部图号（只算换成图的那几个人）。
func subjectRefs(e DimEdit) []int {
	var out []int
	for _, se := range e.Subjects {
		if subjectMode(e, se) == ModeReplace {
			out = append(out, se.Refs...)
		}
	}
	return out
}

// hasSubjectReplace 这一项有没有「换成图」的按人指定。
func hasSubjectReplace(e DimEdit) bool { return len(subjectRefs(e)) > 0 }

// subjectPresentIn 人物在 [t0,t1) 里是否出镜；分析没给出镜时段 = 不知道，当作在。
func subjectPresentIn(s Subject, t0, t1 float64) bool {
	if len(s.Segments) == 0 {
		return true
	}
	for _, w := range s.Segments {
		if w.T1 > t0 && w.T0 < t1 {
			return true
		}
	}
	return false
}

// subjectPresentAt 人物在时刻 t 是否出镜（定妆图用）；没给时段 = 在。
func subjectPresentAt(s Subject, t float64) bool {
	if len(s.Segments) == 0 {
		return true
	}
	for _, w := range s.Segments {
		if t >= w.T0 && t <= w.T1 {
			return true
		}
	}
	return false
}

// subjectEditsOrdered 规范化顺序：按分析里的人物顺序排（同一输入同一指纹，与点名顺序无关）。
func subjectEditsOrdered(an *AnalysisResult, es []SubjectEdit) []SubjectEdit {
	pos := map[string]int{}
	for i, s := range an.Subjects {
		pos[s.ID] = i
	}
	out := append([]SubjectEdit(nil), es...)
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i].ID] < pos[out[j].ID] })
	return out
}

// validateSubjects 校验并规范化按人点名（CONTRACT §X）。nRefs = 已放参考图张数；occ = 已校验过的逐套标注（查冲突）。
// 缺项：SUBJECT_UNSUPPORTED / SUBJECT_SINGLE / SUBJECT_UNKNOWN / SUBJECT_DUPLICATE / SUBJECT_MODE /
// SUBJECT_OCCURRENCE_CONFLICT，以及复用的 REF_MISSING / REF_UNKNOWN / NOTE_MISSING / NOTE_TOO_LONG。
// 返回规范化结果（mode 显式、keep 条目丢掉、按分析里的人物顺序）；可能为空（全是保持不变）。
func validateSubjects(key, label, field string, e DimEdit, an *AnalysisResult, nRefs int, occ []OccurrenceEdit, add func(code, field, msg string)) []SubjectEdit {
	if !subjectsSupported(key) {
		add("SUBJECT_UNSUPPORTED", field, label+"：只有「人物」和「服装」可以按人指定")
		return nil
	}
	if n := len(an.Subjects); n < 2 {
		if n == 1 {
			add("SUBJECT_SINGLE", field, label+"：视频里只有 1 个人，直接选整项的改法就行，不用按人指定")
			return nil
		}
		add("SUBJECT_UNKNOWN", field, label+"：视频里没有识别出人物，不能按人指定")
		return nil
	}
	seen := map[string]bool{}
	listed := map[string]bool{}
	var out []SubjectEdit
	for i, x := range e.Subjects {
		sf := fmt.Sprintf("%s.subjects[%d]", field, i)
		id := strings.TrimSpace(x.ID)
		sub, ok := subjectByID(an, id)
		if !ok {
			add("SUBJECT_UNKNOWN", sf, label+"：视频里没有这个人物，请重新选择")
			continue
		}
		if seen[id] {
			add("SUBJECT_DUPLICATE", sf, fmt.Sprintf("%s：「%s」重复了，每个人只能指定一次", label, sub.Name))
			continue
		}
		seen[id] = true
		listed[id] = true
		mode := x.Mode
		if mode == "" {
			mode = e.Mode
		}
		note := strings.TrimSpace(x.Note)
		switch mode {
		case ModeKeep:
			continue // 明说保持不变 = 没点名
		case ModeReplace:
			if len(x.Refs) == 0 {
				switch key {
				case "model":
					add("SUBJECT_REFS_MISSING", sf, fmt.Sprintf("换掉「%s」需要一张人物正脸照，生成才稳定", sub.Name))
				default:
					add("SUBJECT_REFS_MISSING", sf, fmt.Sprintf("换「%s」的服装需要放一张图", sub.Name))
				}
			}
			for _, r := range x.Refs {
				if r < 1 || r > nRefs {
					add("REF_UNKNOWN", sf, fmt.Sprintf("%s：「%s」用到的图不在已放的图里，请重新选择", label, sub.Name))
					break
				}
			}
			out = append(out, SubjectEdit{ID: id, Refs: x.Refs, Mode: ModeReplace, Note: note})
		case ModeDescribe:
			if note == "" {
				note = strings.TrimSpace(e.Note) // 自己没写就沿用这一项的说明
			}
			if note == "" {
				add("NOTE_MISSING", sf, fmt.Sprintf("%s：「%s」要怎么改，写一句话再继续", label, sub.Name))
			} else if len([]rune(note)) > 200 {
				add("NOTE_TOO_LONG", sf, label+"：说明写短一点（200 字以内）")
			}
			out = append(out, SubjectEdit{ID: id, Mode: ModeDescribe, Note: note})
		default:
			add("SUBJECT_MODE", sf, fmt.Sprintf("%s：「%s」请选择 保持不变 / 换成我的图 / 按我写的改", label, sub.Name))
		}
	}
	// 同一个人既按人又按套指定：套的 subject 是被点名的人
	outfits := outfitsOf(an.Dims[key])
	for _, o := range occ {
		if o.Index >= 1 && o.Index <= len(outfits) && outfits[o.Index-1].Subject != "" && listed[outfits[o.Index-1].Subject] {
			sub, _ := subjectByID(an, outfits[o.Index-1].Subject)
			add("SUBJECT_OCCURRENCE_CONFLICT", field, fmt.Sprintf("%s：「%s」已经按人指定了，不能再按第 %d 套指定同一个人", label, sub.Name, o.Index))
			break
		}
	}
	return subjectEditsOrdered(an, out)
}

// subjectText 方案卡里逐人一行：「左侧白衣女生 → 图 1；男生 保持不变」。没点名的人写「保持不变」。
func subjectText(an *AnalysisResult, e DimEdit) string {
	var parts []string
	for _, s := range an.Subjects {
		se := subjectEditOf(e, s.ID)
		switch {
		case se == nil || subjectMode(e, *se) == ModeKeep:
			parts = append(parts, s.Name+" 保持不变")
		case subjectMode(e, *se) == ModeDescribe:
			parts = append(parts, fmt.Sprintf("%s → 改为「%s」", s.Name, trimRunes(se.Note, 20)))
		default:
			parts = append(parts, fmt.Sprintf("%s → %s", s.Name, imgNames(se.Refs)))
		}
	}
	return strings.Join(parts, "；")
}

// subjectSlice 某个人物在某一段里的样子。
type subjectSlice struct {
	S    Subject
	Edit *SubjectEdit // nil = 没点名，保持原样
	Mode string       // Edit 非空时的生效 mode
	Span string       // 「本段第 a–b 秒」，空 = 整段都在或不知道
}

// subjectsInSegment 本段出镜的各个人物（按分析里的顺序）。
func subjectsInSegment(an *AnalysisResult, e DimEdit, seg segment.Segment) []subjectSlice {
	var out []subjectSlice
	for _, s := range an.Subjects {
		if !subjectPresentIn(s, seg.GenT0, seg.GenT1) {
			continue
		}
		sl := subjectSlice{S: s, Edit: subjectEditOf(e, s.ID)}
		if sl.Edit != nil {
			sl.Mode = subjectMode(e, *sl.Edit)
		}
		if len(s.Segments) > 0 {
			lo, hi := seg.GenT1, seg.GenT0
			for _, w := range s.Segments {
				if w.T1 > seg.GenT0 && w.T0 < seg.GenT1 {
					lo, hi = minF(lo, maxF(w.T0, seg.GenT0)), maxF(hi, minF(w.T1, seg.GenT1))
				}
			}
			if lo > seg.GenT0 || hi < seg.GenT1 {
				sl.Span = fmt.Sprintf("本段第 %.1f–%.1f 秒出现", lo-seg.GenT0, hi-seg.GenT0)
			}
		}
		out = append(out, sl)
	}
	return out
}

// changed 这个人在本段是否真的要改（换成图 / 按文字改）。
func (sl subjectSlice) changed() bool {
	if sl.Edit == nil {
		return false
	}
	switch sl.Mode {
	case ModeReplace:
		return len(sl.Edit.Refs) > 0
	case ModeDescribe:
		return strings.TrimSpace(sl.Edit.Note) != ""
	}
	return false
}

// fillSubjects 按人点名：按本段出镜的人逐个写指令（被点名的人写换法，没点名的人「保持原样」）；
// 同时带逐套标注时，逐套只写「不属于被点名的人」的那几套（校验已保证两者不冲突）。
// 返回本段占用了几个「主体」编号（replace 的人 + replace 的套）；0 = 本段没有要改的，调用方按「不改」处理。
func fillSubjects(d *Dialect, cv *changeView, an *AnalysisResult, dim Dim, e DimEdit, seg segment.Segment, refCount int) int {
	slices := subjectsInSegment(an, e, seg)
	changed := 0
	for _, sl := range slices {
		if sl.changed() {
			changed++
		}
	}
	var occSlices []outfitSlice
	occReplaced := 0
	if len(e.Occurrences) > 0 {
		for _, o := range outfitsInSegment(dim, e, seg) {
			if o.Subject != "" && subjectEditOf(e, o.Subject) != nil {
				continue
			}
			occSlices = append(occSlices, o)
			if len(o.Refs) > 0 {
				occReplaced++
			}
		}
	}
	if changed == 0 && occReplaced == 0 {
		return 0
	}
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
	for _, sl := range slices {
		if sl.changed() && sl.Mode == ModeReplace {
			all = append(all, sl.Edit.Refs...)
		}
	}
	en := d.Code == "minimax-h3"
	model := cv.Key == "model"
	// 「保持原样」的宾语：人物项直接写人，其它项写「的服装」
	of := "的" + cv.Label
	if model {
		of = ""
	}
	var parts, keeps, retain []string
	subj := cv.SubjNo
	for _, sl := range slices {
		name := sl.S.Name
		if !sl.changed() {
			keeps = append(keeps, name)
			continue
		}
		span := ""
		if sl.Span != "" {
			span = "（" + sl.Span + "）"
			if en {
				span = " (" + sl.Span + ")"
			}
		}
		if sl.Mode == ModeDescribe {
			note := strings.TrimSpace(sl.Edit.Note)
			if en {
				parts = append(parts, fmt.Sprintf("Change the %s of \"%s\"%s as follows: %s", cv.Noun, name, span, note))
			} else {
				parts = append(parts, fmt.Sprintf("「%s」%s的%s改为：%s", name, span, cv.Label, note))
			}
			continue
		}
		tags := tagOf(sl.Edit.Refs)
		switch {
		case en && model:
			parts = append(parts, fmt.Sprintf("<Subject %d> is the new person in %s; it replaces the person \"%s\" in the source%s. Only the %s are referenced; background, pose and watermark in the picture are ignored.",
				subj, tags, name, span, cv.Attrs))
			retain = append(retain, fmt.Sprintf("<Subject %d>: identity must stay consistent in every shot.", subj))
		case en:
			parts = append(parts, fmt.Sprintf("<Subject %d> is the new %s in %s; it replaces the %s worn by \"%s\" in the source%s. Only the %s are referenced; background, pose and watermark in the picture are ignored.",
				subj, cv.Noun, tags, cv.Noun, name, span, cv.Attrs))
			retain = append(retain, fmt.Sprintf("<Subject %d>: identity must stay consistent in every shot.", subj))
		case d.Code == "seedance-2" && model:
			parts = append(parts, fmt.Sprintf("将 %s 中的人物定义为<主体%d>，用于替换原片中的「%s」%s，%s", tags, subj, name, span, cv.Hint))
		case d.Code == "seedance-2":
			parts = append(parts, fmt.Sprintf("将 %s 中的服装定义为<主体%d>，用于替换原片中「%s」身上的服装%s，%s", tags, subj, name, span, cv.Hint))
		case model:
			parts = append(parts, fmt.Sprintf("视频中的「%s」%s换成 %s 中的人物，%s", name, span, tags, cv.Hint))
		default:
			parts = append(parts, fmt.Sprintf("「%s」%s身上的服装换成 %s 中的服装，%s", name, span, tags, cv.Hint))
		}
		subj++
	}
	var text string
	switch {
	case en:
		text = strings.Join(parts, "\n")
		if len(keeps) > 0 {
			text += fmt.Sprintf("\nKeep \"%s\" exactly as in the source; do not replace.", strings.Join(keeps, "\", \""))
		}
	case d.Code == "seedance-2":
		text = strings.Join(parts, "。\n") + "。"
		if len(keeps) > 0 {
			text += fmt.Sprintf("\n「%s」%s保持原样，不要替换。", strings.Join(keeps, "」「"), of)
		}
	default:
		head := "视频里有多个人，逐个处理："
		if !model {
			head = "视频里有多个人，" + cv.Label + "逐个处理："
		}
		text = head + strings.Join(parts, "；")
		if len(keeps) > 0 {
			text += fmt.Sprintf("；「%s」%s保持原样，不要替换", strings.Join(keeps, "」「"), of)
		}
		if d.Code != "wan-3.0" { // 万相模板自己收句号
			text += "。"
		}
	}
	// 逐套（未按人指定的部分）：复用逐套写法，备注由本函数统一补一次
	if len(occSlices) > 0 {
		oc := *cv
		oc.SubjNo, oc.Note = subj, ""
		fillOutfits(d, &oc, occSlices, refCount)
		for _, o := range occSlices {
			all = append(all, o.Refs...)
		}
		switch {
		case en, d.Code == "seedance-2":
			text += "\n" + oc.OccText
		case d.Code == "wan-3.0":
			text += "；" + oc.OccText
		default:
			text += oc.OccText
		}
		if oc.Retain != "" {
			retain = append(retain, oc.Retain)
		}
	}
	if n := strings.TrimSpace(cv.Note); n != "" && !en {
		text += "补充要求：" + n + "。"
	}
	cv.OccText, cv.PerSubject = text, true
	cv.Tags = strings.Join(uniqTags(all, tagOf), "、")
	cv.ShotText = "各人物按上面逐个指定的处理，没指定的人物保持原样"
	if !model {
		cv.ShotText = "各人物的" + cv.Label + "按上面逐个指定的处理，没指定的人物保持原样"
	}
	if len(retain) > 0 {
		cv.Retain = strings.Join(retain, "\n")
	}
	return changed + occReplaced
}

// previewSubjectLines 定妆图里按人点名的几行（图号已按「图 1 = 原片帧」顺延）：只写在这一帧里出现的人；
// ignorePresence = 一个人都对不上时的兜底（宁可多写，也不让定妆图什么都没改）。
func previewSubjectLines(key string, e DimEdit, an *AnalysisResult, nRefs int, frameT float64, ignorePresence bool) []string {
	label := DimLabel[key]
	var lines []string
	for _, s := range an.Subjects {
		se := subjectEditOf(e, s.ID)
		if se == nil || (!ignorePresence && !subjectPresentAt(s, frameT)) {
			continue
		}
		switch subjectMode(e, *se) {
		case ModeReplace:
			var refs []string
			for _, r := range se.Refs {
				if r >= 1 && r <= nRefs {
					refs = append(refs, fmt.Sprintf("图%d", r+1))
				}
			}
			if len(refs) == 0 {
				continue
			}
			var l string
			if key == "model" {
				l = fmt.Sprintf("把画面里的「%s」换成%s里的人物", s.Name, strings.Join(refs, "、"))
			} else {
				l = fmt.Sprintf("把画面里「%s」身上的服装换成%s里的服装", s.Name, strings.Join(refs, "、"))
			}
			if h := refHint[key]; h != "" {
				l += "（" + h + "）"
			}
			if n := strings.TrimSpace(se.Note); n != "" {
				l += "；补充要求：" + n
			}
			lines = append(lines, l+"；画面里的其他人物保持原样")
		case ModeDescribe:
			if n := strings.TrimSpace(se.Note); n != "" {
				lines = append(lines, fmt.Sprintf("把画面里「%s」的%s改成：%s；画面里的其他人物保持原样", s.Name, label, n))
			}
		}
	}
	return lines
}

// SubjectTexts 方案里按人点名的逐行说明（后台任务详情用）：与 PlanView.changes[].subjectText 同一口径
// （只收「换成我的图 / 按我写的改」且点了人的维度，按分析里的维度顺序，每个维度一行）。没点名返回 nil。
func SubjectTexts(an *AnalysisResult, ann Annotations) []string {
	if an == nil {
		return nil
	}
	var out []string
	for _, k := range an.DimOrder {
		e, ok := ann.Dims[k]
		if !ok || len(e.Subjects) == 0 || (e.Mode != ModeReplace && e.Mode != ModeDescribe) {
			continue
		}
		if t := subjectText(an, e); t != "" {
			// 带维度前缀（「人物：…」「服装：…」）：管理端同时有换人与换装时要分得清哪行是哪项
			if label := DimLabel[k]; label != "" {
				t = label + "：" + t
			}
			out = append(out, t)
		}
	}
	return out
}
