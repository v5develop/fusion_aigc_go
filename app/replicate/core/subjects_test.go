package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/analysisfix"
	"aigc-local-life/app/replicate/rules/segment"
)

// ---- 多人点名（CONTRACT §X）----

var twoSubjects = []Subject{
	{ID: "p1", Name: "左侧白衣女生", Desc: "长直发 白色卫衣", Segments: []TimeSpan{{T0: 0, T1: 20}}},
	{ID: "p2", Name: "右侧黑衣男生", Desc: "短发 黑色夹克", Segments: []TimeSpan{{T0: 14, T1: 32}}},
}

// seedSubjectAnalysis 两个人的分析：32 秒，镜头切点 12 / 25（拆 3 段）；p1 只在前两段、p2 只在后两段出镜。
func (e *env) seedSubjectAnalysis(subs []Subject, occ []Occurrence) *model.Analysis {
	return e.seedSubjectAnalysisD(32, subs, occ, 12, 25)
}

func (e *env) seedSubjectAnalysisD(d float64, subs []Subject, occ []Occurrence, cuts ...float64) *model.Analysis {
	r := AnalysisResult{PromptVersion: AnalysisPromptVersion, DurationS: d, Width: 720, Height: 1280, HasAudio: true, AudioKind: "speech",
		Dims: map[string]Dim{}, DimOrder: DimKeys, Subjects: subs}
	for _, k := range DimKeys {
		r.Dims[k] = Dim{Present: true, Desc: DimLabel[k] + "现状", Risk: "low"}
	}
	g := r.Dims["garment"]
	g.Occurrences = occ
	r.Dims["garment"] = g
	prev := 0.0
	for i, c := range append(cuts, d) {
		r.Sections = append(r.Sections, Section{Index: i + 1, T0: prev, T1: c, Desc: fmt.Sprintf("镜头%d", i+1)})
		prev = c
	}
	b, _ := json.Marshal(r)
	no := fmt.Sprintf("VRAsubj%v-%d-%d", d, len(subs), len(occ))
	a, _, _ := e.store.InsertAnalysis(e.ctx, &model.Analysis{AnalysisNo: no, UserID: uid, SourceKey: no,
		SourceURL: "https://x/v.mp4", SourceRid: ptr("src-rid"), Status: model.AnalysisSucceeded, Stage: model.StageDone, Result: b})
	return a
}

func (e *env) planSubj(a *model.Analysis, group string, dims map[string]DimEdit) (*PlanView, error) {
	return e.planOutfit(a, group, dims)
}

func modelSubj(subs ...SubjectEdit) map[string]DimEdit {
	return map[string]DimEdit{"model": {Mode: ModeReplace, Subjects: subs}}
}

// 没用按人点名：DimEdit / Annotations 的 JSON 不出现 subjects，字节与旧版结构一致（plan_hash 不变）；
// 分析里有多个人但方案没点名时，提示词也与以前一致（不出现人物称呼）。
func TestSubjectsAbsentKeepsJSONAndPlanHash(t *testing.T) {
	type legacyEdit struct {
		Mode        string           `json:"mode"`
		Note        string           `json:"note,omitempty"`
		Refs        []int            `json:"refs,omitempty"`
		Occurrences []OccurrenceEdit `json:"occurrences,omitempty"`
	}
	type legacyAnn struct {
		Dims  map[string]legacyEdit `json:"dims"`
		Audio string                `json:"audio"`
		Texts []TextOverlay         `json:"texts,omitempty"`
	}
	for _, tc := range []struct {
		n DimEdit
		o legacyEdit
	}{
		{DimEdit{Mode: ModeReplace, Refs: []int{1}, Note: "x"}, legacyEdit{Mode: ModeReplace, Refs: []int{1}, Note: "x"}},
		{DimEdit{Mode: ModeReplace, Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{2}}}}, legacyEdit{Mode: ModeReplace, Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{2}}}}},
		{DimEdit{Mode: ModeKeep}, legacyEdit{Mode: ModeKeep}},
	} {
		a, _ := json.Marshal(Annotations{Dims: map[string]DimEdit{"model": tc.n}, Audio: AudioRegenerate})
		b, _ := json.Marshal(legacyAnn{Dims: map[string]legacyEdit{"model": tc.o}, Audio: AudioRegenerate})
		if string(a) != string(b) || strings.Contains(string(a), "subjects") {
			t.Fatalf("未用按人点名时 JSON 必须与旧版一致：\n%s\n%s", a, b)
		}
		if planHash("A", "v", "r", Annotations{Dims: map[string]DimEdit{"model": tc.n}}, nil, "g", "720p", "", nil, nil) !=
			planHash("A", "v", "r", Annotations{Dims: map[string]DimEdit{"model": tc.n}}, nil, "g", "720p", "", nil, nil) {
			t.Fatal("指纹应稳定")
		}
	}
	e := newEnv(t, true)
	a := e.seedSubjectAnalysis(twoSubjects, nil)
	pv, err := e.planSubj(a, "vid.seedance-2.5", map[string]DimEdit{"model": {Mode: ModeReplace, Refs: []int{1}}})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(pv.Prompts, "\n")
	if strings.Contains(all, "左侧白衣女生") || strings.Contains(all, "逐个处理") {
		t.Fatalf("没点名的方案提示词不应出现按人写法：\n%s", all)
	}
	if pv.Changes[0].SubjectText != "" || len(pv.Changes[0].Subjects) != 0 {
		t.Fatalf("没点名：方案卡不带 subjectText：%+v", pv.Changes[0])
	}
	// 点名会改变指纹
	pv2, err := e.planSubj(a, "vid.seedance-2.5", modelSubj(SubjectEdit{ID: "p1", Refs: []int{1}}))
	if err != nil || pv2.PlanHash == pv.PlanHash {
		t.Fatalf("点名应改变指纹：%v", err)
	}
}

func TestSubjectValidation(t *testing.T) {
	e := newEnv(t, true)
	two := e.seedSubjectAnalysis(twoSubjects, nil)
	one := e.seedSubjectAnalysis(twoSubjects[:1], nil)
	outfitsOfSubj := []Occurrence{{T0: 0, T1: 10, Desc: "白衬衫", Subject: "p1"}, {T0: 10, T1: 20, Desc: "黑西装", Subject: "p2"}, {T0: 20, T1: 32, Desc: "红裙子"}}
	withOcc := e.seedSubjectAnalysis(twoSubjects, outfitsOfSubj)
	rep := func(k string, subs ...SubjectEdit) map[string]DimEdit {
		return map[string]DimEdit{k: {Mode: ModeReplace, Subjects: subs}}
	}
	cases := []struct {
		name string
		a    *model.Analysis
		dims map[string]DimEdit
		want string // 空 = 通过
	}{
		{"合法-换人", two, rep("model", SubjectEdit{ID: "p1", Refs: []int{1}}), ""},
		{"合法-两个人各换", two, rep("model", SubjectEdit{ID: "p2", Refs: []int{2}}, SubjectEdit{ID: "p1", Refs: []int{1}}), ""},
		{"合法-换装", two, rep("garment", SubjectEdit{ID: "p2", Refs: []int{2}}), ""},
		{"未知人物", two, rep("model", SubjectEdit{ID: "p9", Refs: []int{1}}), "SUBJECT_UNKNOWN"},
		{"人物重复", two, rep("model", SubjectEdit{ID: "p1", Refs: []int{1}}, SubjectEdit{ID: "p1", Refs: []int{2}}), "SUBJECT_DUPLICATE"},
		{"缺图", two, rep("model", SubjectEdit{ID: "p1"}), "SUBJECT_REFS_MISSING"},
		{"图号越界", two, rep("garment", SubjectEdit{ID: "p1", Refs: []int{3}}), "REF_UNKNOWN"},
		{"只有一个人不允许", one, rep("model", SubjectEdit{ID: "p1", Refs: []int{1}}), "SUBJECT_SINGLE"},
		{"商品不支持按人", two, rep("product", SubjectEdit{ID: "p1", Refs: []int{1}}), "SUBJECT_UNSUPPORTED"},
		{"人物 mode 不合法", two, rep("model", SubjectEdit{ID: "p1", Refs: []int{1}, Mode: "x"}), "SUBJECT_MODE"},
		{"这一项不改却点名", two, map[string]DimEdit{"model": {Mode: ModeKeep, Subjects: []SubjectEdit{{ID: "p1", Refs: []int{1}}}}}, "SUBJECT_DIM_MODE"},
		{"describe 缺说明", two, map[string]DimEdit{"garment": {Mode: ModeDescribe, Subjects: []SubjectEdit{{ID: "p1"}}}}, "NOTE_MISSING"},
		{"describe 合法", two, map[string]DimEdit{"garment": {Mode: ModeDescribe, Subjects: []SubjectEdit{{ID: "p1", Note: "改成红色连衣裙"}}}}, ""},
		{"describe 沿用这一项的说明", two, map[string]DimEdit{"garment": {Mode: ModeDescribe, Note: "改成红色", Subjects: []SubjectEdit{{ID: "p1"}}}}, ""},
		{"冲突-同一人既按人又按套", withOcc, map[string]DimEdit{"garment": {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Refs: []int{1}}},
			Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{2}}}}}, "SUBJECT_OCCURRENCE_CONFLICT"},
		{"不冲突-套属于没点名的人", withOcc, map[string]DimEdit{"garment": {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Refs: []int{1}}},
			Occurrences: []OccurrenceEdit{{Index: 2, Refs: []int{2}}}}}, ""},
		{"不冲突-套没有归属", withOcc, map[string]DimEdit{"garment": {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Refs: []int{1}}},
			Occurrences: []OccurrenceEdit{{Index: 3, Refs: []int{2}}}}}, ""},
		{"全部保持不变 = 没改", two, rep("model", SubjectEdit{ID: "p1", Mode: ModeKeep}), "NOTHING_CHANGED"},
	}
	for _, c := range cases {
		_, err := e.planSubj(c.a, "vid.seedance-2.5", c.dims)
		got := ""
		if err != nil {
			got = gapCodesStr(err)
		}
		if got != c.want {
			t.Errorf("%s：期望 %q 实际 %q", c.name, c.want, got)
		}
	}
	// 规范化：按分析里的人物顺序、mode 显式、keep 条目丢掉、维度级图忽略
	pv, err := e.planSubj(two, "vid.seedance-2.5", map[string]DimEdit{"model": {Mode: ModeReplace, Refs: []int{2}, Subjects: []SubjectEdit{
		{ID: "p2", Refs: []int{2}}, {ID: "p1", Refs: []int{1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	m := pv.Annotations.Dims["model"]
	if len(m.Refs) != 0 || len(m.Subjects) != 2 || m.Subjects[0].ID != "p1" || m.Subjects[0].Mode != ModeReplace || m.Subjects[1].ID != "p2" {
		t.Fatalf("规范化：%+v", m)
	}
	pv, err = e.planSubj(two, "vid.seedance-2.5", rep("model", SubjectEdit{ID: "p2", Mode: ModeKeep}, SubjectEdit{ID: "p1", Refs: []int{1}}))
	if err != nil || len(pv.Annotations.Dims["model"].Subjects) != 1 {
		t.Fatalf("keep 条目应丢掉：%v %+v", err, pv)
	}
}

// 两个人、一个只在前两段、一个只在后两段：逐段只写出镜的人；被点名的人不在的段，这一项不改。
func TestSubjectPromptPerSegment(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedSubjectAnalysis(twoSubjects, nil)
	// 两个人都换
	pv, err := e.planSubj(a, "vid.seedance-2.5", modelSubj(SubjectEdit{ID: "p1", Refs: []int{1}}, SubjectEdit{ID: "p2", Refs: []int{2}}))
	if err != nil {
		t.Fatal(err)
	}
	p := pv.Prompts
	if len(p) < 3 {
		t.Fatalf("应拆成 3 段：%d", len(p))
	}
	if !strings.Contains(p[0], "「左侧白衣女生」") || !strings.Contains(p[0], "@图片1") || strings.Contains(p[0], "右侧黑衣男生") || strings.Contains(p[0], "@图片2") {
		t.Fatalf("第 1 段只该有 p1：\n%s", p[0])
	}
	if !strings.Contains(p[1], "左侧白衣女生") || !strings.Contains(p[1], "右侧黑衣男生") || !strings.Contains(p[1], "@图片1") || !strings.Contains(p[1], "@图片2") {
		t.Fatalf("第 2 段两个人都在：\n%s", p[1])
	}
	last := p[len(p)-1]
	if !strings.Contains(last, "「右侧黑衣男生」") || !strings.Contains(last, "@图片2") || strings.Contains(last, "左侧白衣女生") || strings.Contains(last, "@图片1") {
		t.Fatalf("末段只该有 p2：\n%s", last)
	}
	// 只点名 p1：第 2 段 p2 在场 → 写「保持原样」；末段 p1 不在 → 人物这一项按不改（不出现任何图）
	pv, err = e.planSubj(a, "vid.seedance-2.5", modelSubj(SubjectEdit{ID: "p1", Refs: []int{1}}))
	if err != nil {
		t.Fatal(err)
	}
	p = pv.Prompts
	if !strings.Contains(p[1], "「右侧黑衣男生」保持原样") || !strings.Contains(p[0], "@图片1") || strings.Contains(p[0], "右侧黑衣男生") {
		t.Fatalf("只点名 p1：\n第1段 %s\n第2段 %s", p[0], p[1])
	}
	last = p[len(p)-1]
	if strings.Contains(last, "@图片") || strings.Contains(last, "左侧白衣女生") || !strings.Contains(last, "人物、") && !strings.Contains(last, "人物参考") {
		t.Fatalf("被点名的人不在的段：人物应按不改处理：\n%s", last)
	}
	// 换装：按人写「身上的服装」
	pv, err = e.planSubj(a, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p2", Refs: []int{2}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pv.Prompts[1], "「右侧黑衣男生」") || !strings.Contains(pv.Prompts[1], "身上的服装换成 @图片2") || !strings.Contains(pv.Prompts[1], "「左侧白衣女生」的服装保持原样") {
		t.Fatalf("按人换装：\n%s", pv.Prompts[1])
	}
	if strings.Contains(pv.Prompts[0], "@图片") {
		t.Fatalf("p2 不在第 1 段：服装这一项不改：\n%s", pv.Prompts[0])
	}
}

func TestSubjectPromptAllDialects(t *testing.T) {
	an := &AnalysisResult{DurationS: 10, DimOrder: DimKeys, Subjects: twoSubjects[:1], Dims: map[string]Dim{"model": {Present: true, Desc: "两个人"}, "garment": {Present: true, Desc: "服装"}},
		Sections: []Section{{Index: 1, T0: 0, T1: 10, Desc: "对话"}}}
	an.Subjects = []Subject{{ID: "p1", Name: "左侧白衣女生", Desc: "x"}, {ID: "p2", Name: "男生", Desc: "y"}} // 无出镜时段 = 每段都在
	seg := segment.Segment{Index: 1, GenT0: 0, GenT1: 10, GenDuration: 10}
	for _, g := range []string{"vid.seedance-2.5", "vid.seedance-2", "vid.minimax-h3", "vid.wan-3.0", "vid.other"} {
		for _, k := range []string{"model", "garment"} {
			ann := Annotations{Audio: AudioRegenerate, Dims: map[string]DimEdit{k: {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Mode: ModeReplace, Refs: []int{1}}}}}}
			d := DialectFor(g)
			out := RenderDialect(d, an, ann, 1, seg, 1)
			if !strings.Contains(out, fmt.Sprintf(d.ImageFmt, 1)) || !strings.Contains(out, "左侧白衣女生") || !strings.Contains(out, "男生") {
				t.Errorf("%s/%s：缺逐人写法：\n%s", g, k, out)
			}
			if strings.Contains(out, "<no value>") || strings.Contains(out, "%!") {
				t.Errorf("%s/%s：模板渲染异常：\n%s", g, k, out)
			}
		}
	}
	// describe 模式
	ann := Annotations{Audio: AudioRegenerate, Dims: map[string]DimEdit{"garment": {Mode: ModeDescribe, Subjects: []SubjectEdit{{ID: "p2", Mode: ModeDescribe, Note: "改成红色连衣裙"}}}}}
	for _, g := range []string{"vid.seedance-2.5", "vid.minimax-h3"} {
		if out := RenderDialect(DialectFor(g), an, ann, 0, seg, 1); !strings.Contains(out, "改成红色连衣裙") || !strings.Contains(out, "男生") {
			t.Errorf("%s describe：\n%s", g, out)
		}
	}
}

// 按人点名 + 逐套换装并存：逐套只写不属于被点名的人的那几套。
func TestSubjectWithOccurrencesRendering(t *testing.T) {
	an := &AnalysisResult{DurationS: 20, DimOrder: DimKeys, Subjects: []Subject{{ID: "p1", Name: "女生"}, {ID: "p2", Name: "男生"}},
		Dims: map[string]Dim{"garment": {Present: true, Desc: "服装", Occurrences: []Occurrence{
			{T0: 0, T1: 10, Desc: "白衬衫", Subject: "p1"}, {T0: 0, T1: 10, Desc: "黑西装", Subject: "p2"}, {T0: 10, T1: 20, Desc: "红裙子"}}}},
		Sections: []Section{{Index: 1, T0: 0, T1: 20, Desc: "走秀"}}}
	ann := Annotations{Audio: AudioRegenerate, Dims: map[string]DimEdit{"garment": {Mode: ModeReplace,
		Subjects:    []SubjectEdit{{ID: "p1", Mode: ModeReplace, Refs: []int{1}}},
		Occurrences: []OccurrenceEdit{{Index: 3, Refs: []int{2}}}}}}
	seg := segment.Segment{Index: 1, GenT0: 0, GenT1: 20, GenDuration: 20}
	out := RenderDialect(DialectFor("vid.seedance-2.5"), an, ann, 2, seg, 1)
	if !strings.Contains(out, "「女生」") || !strings.Contains(out, "@图片1") || !strings.Contains(out, "第 3 套") || !strings.Contains(out, "@图片2") {
		t.Fatalf("按人 + 逐套：\n%s", out)
	}
	if strings.Contains(out, "第 1 套") { // 套 1 属于被点名的女生，由按人说了算
		t.Fatalf("被点名的人的套不应再按套写：\n%s", out)
	}
}

func TestSubjectPlanViewTextAndRisks(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedSubjectAnalysis(twoSubjects, nil)
	// 点名
	pv, err := e.planSubj(a, "vid.seedance-2.5", modelSubj(SubjectEdit{ID: "p1", Refs: []int{1}}))
	if err != nil {
		t.Fatal(err)
	}
	var ml *DimLine
	for i := range pv.Changes {
		if pv.Changes[i].Key == "model" {
			ml = &pv.Changes[i]
		}
	}
	if ml == nil || ml.SubjectText != "左侧白衣女生 → 图 1；右侧黑衣男生 保持不变" || len(ml.Subjects) != 1 {
		t.Fatalf("subjectText：%+v", ml)
	}
	risks := strings.Join(pv.Risks, "|")
	if !strings.Contains(risks, "只换你指定的人，其他人保持原样（人物较近或互相遮挡时可能串）") || strings.Contains(risks, "替换画面里的所有人") || strings.Contains(risks, "你选中的那位") {
		t.Fatalf("点名后的风险文案：%v", pv.Risks)
	}
	// 没点名
	pv, err = e.planSubj(a, "vid.seedance-2.5", map[string]DimEdit{"model": {Mode: ModeReplace, Refs: []int{1}}})
	if err != nil {
		t.Fatal(err)
	}
	risks = strings.Join(pv.Risks, "|")
	if !strings.Contains(risks, "视频里有 2 个人，没有指定的话会尝试替换画面里的所有人") || strings.Contains(risks, "只换你指定的人") {
		t.Fatalf("没点名的风险文案：%v", pv.Risks)
	}
	// 没动人物 / 服装：不提多人风险
	pv, err = e.planSubj(a, "vid.seedance-2.5", map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(pv.Risks, "|"), "个人") {
		t.Fatalf("只换商品不提多人风险：%v", pv.Risks)
	}
	// 人物点名 + 服装没点名：两句都出现
	pv, err = e.planSubj(a, "vid.seedance-2.5", map[string]DimEdit{"model": {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Refs: []int{1}}}},
		"garment": {Mode: ModeReplace, Refs: []int{2}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := strings.Join(pv.Risks, "|"); !strings.Contains(r, "没有指定的话") || !strings.Contains(r, "只换你指定的人") {
		t.Fatalf("混合：%v", pv.Risks)
	}
}

// 批量：主替换项的图位置含各人物 refs 的并集；变体按图号升序依次替换。
func TestBatchWithSubjects(t *testing.T) {
	e := newEnv(t, false)
	mb := &memBatch{e: e}
	WithBatch(mb)(e.svc)
	a := e.seedSubjectAnalysisD(12.3, []Subject{{ID: "p1", Name: "左侧白衣女生"}, {ID: "p2", Name: "右侧黑衣男生"}}, nil)
	pv, err := e.planSubj(a, "vid.seedance-2.5", modelSubj(SubjectEdit{ID: "p1", Refs: []int{1}}, SubjectEdit{ID: "p2", Refs: []int{2}}))
	if err != nil {
		t.Fatal(err)
	}
	if jv := e.run(e.confirm(pv, "", "parent-subj").JobID, 4); jv.Status != "succeeded" {
		t.Fatalf("源任务应成功：%s", jv.Status)
	}
	vs := []BatchVariant{variant("va", "vb"), variant("vc", "vd")}
	q, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, vs)
	if err != nil {
		t.Fatal(err)
	}
	if q.MainDim != "model" || q.RefsPerVariant != 2 {
		t.Fatalf("主替换项按各人物引用的图数：%+v", q)
	}
	// 少给一张图 → 拒收
	if _, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va")}); err == nil {
		t.Fatal("图数应等于各人物图位置的并集")
	}
	bv, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, Variants: vs, ExpectedPoints: q.Total, IdempotencyKey: "b-subj"})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.store.GetJob(e.ctx, bv.Jobs[0].JobID)
	cp, _ := e.store.GetPlan(e.ctx, j.PlanID)
	var ann Annotations
	_ = json.Unmarshal(cp.Annotations, &ann)
	var refs []RefImage
	_ = json.Unmarshal(cp.Refs, &refs)
	if len(ann.Dims["model"].Subjects) != 2 || len(refs) != 2 || refs[0].Rid != testRid("va") || refs[1].Rid != testRid("vb") {
		t.Fatalf("子方案保留按人点名且图换成变体的图：%+v %+v", ann.Dims["model"], refs)
	}
}

func TestSubjectPreviewPrompt(t *testing.T) {
	an := &AnalysisResult{Subjects: twoSubjects, Dims: map[string]Dim{"model": {Present: true, Desc: "两个人"}, "garment": {Present: true, Desc: "服装"}}}
	edits := []previewEdit{
		{Key: "model", Edit: DimEdit{Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Mode: ModeReplace, Refs: []int{1}}, {ID: "p2", Mode: ModeReplace, Refs: []int{2}}}}},
	}
	// 第 5 秒：只有 p1 在画面里
	out := buildPreviewPrompt(FrameOut{T: 5}, edits, an, 2)
	if !strings.Contains(out, "把画面里的「左侧白衣女生」换成图2里的人物") || strings.Contains(out, "右侧黑衣男生") || !strings.Contains(out, "其他人物保持原样") {
		t.Fatalf("第 5 秒：\n%s", out)
	}
	// 第 18 秒：两个人都在
	out = buildPreviewPrompt(FrameOut{T: 18}, edits, an, 2)
	if !strings.Contains(out, "「左侧白衣女生」换成图2") || !strings.Contains(out, "「右侧黑衣男生」换成图3") {
		t.Fatalf("第 18 秒：\n%s", out)
	}
	// 换装只点 p2，但这一帧 p2 不在 → 兜底仍写出（不让定妆图什么都没改）
	g := []previewEdit{{Key: "garment", Edit: DimEdit{Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p2", Mode: ModeReplace, Refs: []int{1}}}}}}
	out = buildPreviewPrompt(FrameOut{T: 2}, g, an, 1)
	if !strings.Contains(out, "「右侧黑衣男生」身上的服装换成图2里的服装") {
		t.Fatalf("兜底：\n%s", out)
	}
	// 没点名：与旧写法一致
	old := buildPreviewPrompt(FrameOut{T: 5}, []previewEdit{{Key: "model", Edit: DimEdit{Mode: ModeReplace, Refs: []int{1}}}}, an, 1)
	if !strings.Contains(old, "1. 把画面里的人物换成图2里的（只参考脸、发型与体态，服装沿用原片）（原画面里是：两个人）") {
		t.Fatalf("未点名的旧写法应不变：\n%s", old)
	}
}

func TestSubjectWriterBrief(t *testing.T) {
	an := &AnalysisResult{DurationS: 32, DimOrder: DimKeys, Subjects: twoSubjects, Dims: map[string]Dim{"model": {Present: true, Desc: "两个人"}},
		Sections: []Section{{Index: 1, T0: 0, T1: 32, Desc: "对话"}}}
	ann := Annotations{Audio: AudioRegenerate, Dims: map[string]DimEdit{"model": {Mode: ModeReplace, Subjects: []SubjectEdit{{ID: "p1", Mode: ModeReplace, Refs: []int{1}}}}}}
	first := segment.Segment{Index: 1, GenT0: 0, GenT1: 12, GenDuration: 12}
	d := DialectFor("vid.seedance-2.5")
	b := writerBrief(d, an, ann, 1, first, 3, RenderDialect(d, an, ann, 1, first, 3))
	if !strings.Contains(b, "【出镜主体】左侧白衣女生：长直发 白色卫衣") || strings.Contains(b, "右侧黑衣男生") || !strings.Contains(b, "按人物逐个处理人物") {
		t.Fatalf("写手简报只列本段出镜的人并逐人转述：\n%s", b)
	}
}

// 分析解析：人物出镜时段宽容解析、夹紧、重名补区分；旧分析（没有 segments）照常可用；用户视图 desc ≤ 20 字。
func TestAnalysisSubjectsParseTolerance(t *testing.T) {
	st := &AnalysisState{Probe: &ProbeOut{DurationS: 10, Width: 720, Height: 1280, HasAudio: false}}
	parse := func(subjects string) *AnalysisResult {
		t.Helper()
		js := `{"dims":{"model":{"present":true,"desc":"两人"}},"subjects":` + subjects + `,"sections":[{"t0":0,"t1":10,"desc":"x"}]}`
		r, err := NormalizeAnalysis(js, st)
		if err != nil {
			t.Fatalf("不应解析失败：%v", err)
		}
		return r
	}
	r := parse(`[{"id":"p1","name":"左侧白衣女生","desc":"长直发白色卫衣长直发白色卫衣长直发白色卫衣","segments":[{"t0":0,"t1":4.44},{"t0":99,"t1":120},{"t0":6,"t1":3}]},{"id":"p2","name":"左侧白衣女生","desc":"b"}]`)
	if len(r.Subjects) != 2 || len(r.Subjects[0].Segments) != 1 || r.Subjects[0].Segments[0].T1 != 4.4 {
		t.Fatalf("时段夹紧 / 丢无效：%+v", r.Subjects)
	}
	if r.Subjects[0].Name == r.Subjects[1].Name {
		t.Fatalf("重名要补区分：%+v", r.Subjects)
	}
	if len(r.Subjects[1].Segments) != 0 {
		t.Fatal("没给时段 = 空（当作每段都在）")
	}
	v := userAnalysis(r)
	if got := []rune(v.Subjects[0].Desc); len(got) != 20 || len([]rune(r.Subjects[0].Desc)) <= 20 {
		t.Fatalf("用户视图 desc ≤ 20 字、不改原结果：%d %d", len(got), len([]rune(r.Subjects[0].Desc)))
	}
	// 畸形 segments / 缺名 / 缺 id
	r = parse(`[{"id":"p1","segments":"全程"},{"id":"p2","name":"男生","segments":[{"t0":"a"}]},{"name":"没有id"},{"id":"p3","segments":[[0,2]]}]`)
	if len(r.Subjects) != 3 || r.Subjects[0].Name != "人物1" || len(r.Subjects[0].Segments)+len(r.Subjects[1].Segments)+len(r.Subjects[2].Segments) != 0 {
		t.Fatalf("畸形 segments 当没有、缺 id 丢掉：%+v", r.Subjects)
	}
	// 旧分析结果 JSON（没有 segments）读出来照常
	var old AnalysisResult
	if err := json.Unmarshal([]byte(`{"subjects":[{"id":"p1","name":"女主","desc":"长发"}]}`), &old); err != nil || len(old.Subjects) != 1 || len(old.Subjects[0].Segments) != 0 {
		t.Fatalf("旧结果：%v %+v", err, old)
	}
	if b, _ := json.Marshal(old.Subjects); strings.Contains(string(b), "segments") {
		t.Fatalf("没有时段不输出 segments：%s", b)
	}
	// 时间纠偏同样作用在人物时段上（模型给的时间超出时长按规则缩放，不单独漏掉人物）
	cfg := analysisfix.Config{}.Normalize()
	js := `{"dims":{"model":{"present":true}},"subjects":[{"id":"p1","name":"a","segments":[{"t0":8,"t1":2}]}],"sections":[{"t0":0,"t1":10,"desc":"x"}]}`
	rr, err := normalizeAnalysis(js, st, &cfg)
	if err != nil || len(rr.Subjects[0].Segments) != 1 || rr.Subjects[0].Segments[0].T0 != 2 || rr.Subjects[0].Segments[0].T1 != 8 {
		t.Fatalf("反向时段应被互换：%v %+v", err, rr)
	}
}
