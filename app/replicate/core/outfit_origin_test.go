package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/segment"
)

// ---- 逐套换装（CONTRACT §O1）----

// seedOutfitAnalysis 同 seedAnalysis，另给「服装」放几套出现（每套 [t0,t1]）。
func (e *env) seedOutfitAnalysis(d float64, occ []Occurrence, cuts ...float64) *model.Analysis {
	r := AnalysisResult{PromptVersion: AnalysisPromptVersion, DurationS: d, Width: 720, Height: 1280, HasAudio: true, AudioKind: "speech",
		Dims: map[string]Dim{}, DimOrder: DimKeys}
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
	no := fmt.Sprintf("VRAoutfit%v", d)
	a, _, _ := e.store.InsertAnalysis(e.ctx, &model.Analysis{AnalysisNo: no, UserID: uid, SourceKey: no,
		SourceURL: "https://x/v.mp4", SourceRid: ptr("src-rid"), Status: model.AnalysisSucceeded, Stage: model.StageDone, Result: b})
	return a
}

var threeOutfits = []Occurrence{{T0: 0, T1: 10, Desc: "白衬衫"}, {T0: 10, T1: 20, Desc: "黑西装"}, {T0: 20, T1: 32, Desc: "红裙子"}}

func ref2() []string { return []string{testRef, "mr://" + testRid("refb")} }

func (e *env) planOutfit(a *model.Analysis, group string, dims map[string]DimEdit) (*PlanView, error) {
	return e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: group, RefImages: ref2(), Annotations: Annotations{Dims: dims}})
}

func gapCodesStr(err error) string { return strings.Join(gapCodes(err), ",") }

// 没用逐套换装：DimEdit / Annotations 的 JSON 不出现 occurrences，字节与旧版结构一致（plan_hash 不变）。
func TestOccurrencesAbsentKeepsJSONAndPlanHash(t *testing.T) {
	ann := Annotations{Dims: map[string]DimEdit{"garment": {Mode: ModeReplace, Refs: []int{1}, Note: "x"}}, Audio: AudioRegenerate}
	type legacyEdit struct {
		Mode string `json:"mode"`
		Note string `json:"note,omitempty"`
		Refs []int  `json:"refs,omitempty"`
	}
	type legacyAnn struct {
		Dims  map[string]legacyEdit `json:"dims"`
		Audio string                `json:"audio"`
		Texts []TextOverlay         `json:"texts,omitempty"`
	}
	a, _ := json.Marshal(ann)
	b, _ := json.Marshal(legacyAnn{Dims: map[string]legacyEdit{"garment": {Mode: ModeReplace, Refs: []int{1}, Note: "x"}}, Audio: AudioRegenerate})
	if string(a) != string(b) || strings.Contains(string(a), "occurrences") {
		t.Fatalf("未用逐套时 JSON 必须与旧版一致：\n%s\n%s", a, b)
	}
	// 同一输入走完整方案：哈希稳定（含 6 次 UTC 时间无关输入）
	e := newEnv(t, true)
	an := e.seedOutfitAnalysis(32, threeOutfits, 12, 25)
	p1, err := e.planOutfit(an, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace, Refs: []int{1}}})
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := e.planOutfit(an, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace, Refs: []int{1}}})
	if p1.PlanHash != p2.PlanHash {
		t.Fatal("同输入同指纹")
	}
	// 逐套标注会改变指纹，且不影响不标注的方案文案（仍是旧写法：整段一张图）
	if strings.Contains(strings.Join(p1.Prompts, "\n"), "逐套") {
		t.Fatalf("未标注逐套的方案提示词不应出现逐套写法：%v", p1.Prompts)
	}
	if len(p1.Changes) == 0 || p1.Changes[0].OccurrenceText != "" || len(p1.Changes[0].Occurrences) != 0 {
		t.Fatalf("未标注逐套：方案卡不带逐套文案：%+v", p1.Changes)
	}
}

func TestOccurrenceValidation(t *testing.T) {
	e := newEnv(t, true)
	multi := e.seedOutfitAnalysis(32, threeOutfits, 12, 25)
	single := e.seedAnalysis(32, 12, 25) // 服装没有逐套信息
	rep := func(oc ...OccurrenceEdit) map[string]DimEdit {
		return map[string]DimEdit{"garment": {Mode: ModeReplace, Occurrences: oc}}
	}
	cases := []struct {
		name string
		a    *model.Analysis
		dims map[string]DimEdit
		want string // 空 = 通过
	}{
		{"合法", multi, rep(OccurrenceEdit{Index: 1, Refs: []int{1}}, OccurrenceEdit{Index: 3, Refs: []int{2}}), ""},
		{"套号越界", multi, rep(OccurrenceEdit{Index: 4, Refs: []int{1}}), "OCCURRENCE_INDEX"},
		{"套号 0", multi, rep(OccurrenceEdit{Index: 0, Refs: []int{1}}), "OCCURRENCE_INDEX"},
		{"套号重复", multi, rep(OccurrenceEdit{Index: 2, Refs: []int{1}}, OccurrenceEdit{Index: 2, Refs: []int{2}}), "OCCURRENCE_DUPLICATE"},
		{"图号越界", multi, rep(OccurrenceEdit{Index: 1, Refs: []int{3}}), "REF_UNKNOWN"},
		{"缺图", multi, rep(OccurrenceEdit{Index: 1}), "REF_MISSING"},
		{"只有一套不允许", single, rep(OccurrenceEdit{Index: 1, Refs: []int{1}}), "OCCURRENCE_UNSUPPORTED"},
		{"describe 不允许", multi, map[string]DimEdit{"garment": {Mode: ModeDescribe, Note: "改成红色", Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{1}}}}}, "OCCURRENCE_MODE"},
	}
	for _, c := range cases {
		_, err := e.planOutfit(c.a, "vid.seedance-2.5", c.dims)
		got := ""
		if err != nil {
			got = gapCodesStr(err)
		}
		if got != c.want {
			t.Errorf("%s：期望 %q 实际 %q", c.name, c.want, got)
		}
	}
	// 规范化：按套号升序，维度级 refs 被忽略
	pv, err := e.planOutfit(multi, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace, Refs: []int{2},
		Occurrences: []OccurrenceEdit{{Index: 3, Refs: []int{2}}, {Index: 1, Refs: []int{1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	g := pv.Annotations.Dims["garment"]
	if len(g.Refs) != 0 || len(g.Occurrences) != 2 || g.Occurrences[0].Index != 1 || g.Occurrences[1].Index != 3 {
		t.Fatalf("规范化：%+v", g)
	}
}

// 方案卡：逐套文案 + 结构化；提示词按每段重叠的套写，各方言都带「第 N 套」「图」。
func TestOccurrencePromptPerSegment(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedOutfitAnalysis(32, threeOutfits, 12, 25)
	pv, err := e.planOutfit(a, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace,
		Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{1}}, {Index: 3, Refs: []int{2}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var gl *DimLine
	for i := range pv.Changes {
		if pv.Changes[i].Key == "garment" {
			gl = &pv.Changes[i]
		}
	}
	if gl == nil || gl.OccurrenceText != "第 1 套 → 图 1；第 3 套 → 图 2" || len(gl.Occurrences) != 2 {
		t.Fatalf("方案卡逐套文案：%+v", gl)
	}
	if len(pv.Prompts) < 3 {
		t.Fatalf("应拆成 3 段：%d", len(pv.Prompts))
	}
	p := pv.Prompts
	// 第 1 段（原片约 0–12 秒）：套 1（换成图1）+ 套 2（保持）
	if !strings.Contains(p[0], "第 1 套服装") || !strings.Contains(p[0], "@图片1") || !strings.Contains(p[0], "第 2 套服装保持原样") || strings.Contains(p[0], "@图片2") {
		t.Fatalf("第 1 段：\n%s", p[0])
	}
	// 最后一段（约 25–32 秒）：只有套 3（换成图2）
	last := p[len(p)-1]
	if !strings.Contains(last, "第 3 套服装") || !strings.Contains(last, "@图片2") || strings.Contains(last, "第 1 套") || strings.Contains(last, "@图片1") {
		t.Fatalf("末段：\n%s", last)
	}
	// 中间段（约 12–25 秒）：套 2（保持）+ 套 3（换成图2）
	if mid := p[1]; !strings.Contains(mid, "第 3 套服装") || !strings.Contains(mid, "第 2 套服装保持原样") {
		t.Fatalf("中间段：\n%s", mid)
	}
	// 旧的「整段同一张图」写法不再出现
	for i, s := range p {
		if strings.Contains(s, "服装：@图片") {
			t.Errorf("第 %d 段不应保留整段同图写法：\n%s", i+1, s)
		}
	}
	// 一段里一个被换的套都没有：服装按「不改」处理
	pv2, err := e.planOutfit(a, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace, Occurrences: []OccurrenceEdit{{Index: 3, Refs: []int{2}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pv2.Prompts[0], "@图片") || !strings.Contains(pv2.Prompts[0], "服装、") {
		t.Fatalf("没有被换的套的段：服装应保持不变：\n%s", pv2.Prompts[0])
	}
}

func TestOccurrencePromptAllDialects(t *testing.T) {
	an := &AnalysisResult{DurationS: 10, DimOrder: DimKeys, Dims: map[string]Dim{"garment": {Present: true, Desc: "服装", Occurrences: []Occurrence{
		{T0: 0, T1: 5, Desc: "白衬衫"}, {T0: 5, T1: 10, Desc: "黑西装"}}}}, Sections: []Section{{Index: 1, T0: 0, T1: 10, Desc: "走秀"}}}
	ann := Annotations{Audio: AudioRegenerate, Dims: map[string]DimEdit{"garment": {Mode: ModeReplace, Occurrences: []OccurrenceEdit{{Index: 2, Refs: []int{1}}}}}}
	seg := segment.Segment{Index: 1, GenT0: 0, GenT1: 10, GenDuration: 10}
	for _, g := range []string{"vid.seedance-2.5", "vid.seedance-2", "vid.minimax-h3", "vid.wan-3.0", "vid.other"} {
		d := DialectFor(g)
		out := RenderDialect(d, an, ann, 1, seg, 1)
		tag := fmt.Sprintf(d.ImageFmt, 1)
		if !strings.Contains(out, tag) || !(strings.Contains(out, "第 2 套") || strings.Contains(out, "outfit 2")) {
			t.Errorf("%s：缺逐套写法：\n%s", g, out)
		}
		if strings.Contains(out, "<no value>") || strings.Contains(out, "%!") {
			t.Errorf("%s：模板渲染异常：\n%s", g, out)
		}
		if !(strings.Contains(out, "第 1 套") || strings.Contains(out, "outfit 1")) {
			t.Errorf("%s：没被换的套应写保持原样：\n%s", g, out)
		}
	}
}

func TestAnalysisOutfitsSummary(t *testing.T) {
	r := &AnalysisResult{Dims: map[string]Dim{"garment": {Present: true, Occurrences: []Occurrence{
		{T0: 10, T1: 20, Desc: "黑西装"}, {T0: 0, T1: 10, Desc: "白衬衫"}}}}}
	u := userAnalysis(r)
	if u.Outfits == nil || u.Outfits.Count != 2 || u.Outfits.Items[0].Index != 1 || u.Outfits.Items[0].Desc != "白衬衫" || u.Outfits.Items[1].T0 != 10 {
		t.Fatalf("套数摘要（按开始时间升序，套号从 1）：%+v", u.Outfits)
	}
	if r.Outfits != nil {
		t.Fatal("不改原结果（落库口径）")
	}
	if userAnalysis(&AnalysisResult{Dims: map[string]Dim{"garment": {Present: true}}}).Outfits != nil {
		t.Fatal("没有逐套信息：不带 outfits")
	}
	b, _ := json.Marshal(u)
	if !strings.Contains(string(b), `"outfits":{"count":2,"items":[{"index":1,"t0":0,"t1":10,"desc":"白衬衫"}`) {
		t.Fatalf("JSON：%s", b)
	}
}

// 批量：主替换项用逐套换装时，变体的图同时替换各套引用的图位置。
func TestBatchWithOccurrences(t *testing.T) {
	e := newEnv(t, false)
	mb := &memBatch{e: e}
	WithBatch(mb)(e.svc)
	a := e.seedOutfitAnalysis(12.3, []Occurrence{{T0: 0, T1: 6, Desc: "白衬衫"}, {T0: 6, T1: 12.3, Desc: "黑西装"}})
	pv, err := e.planOutfit(a, "vid.seedance-2.5", map[string]DimEdit{"garment": {Mode: ModeReplace,
		Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{1}}, {Index: 2, Refs: []int{2}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if jv := e.run(e.confirm(pv, "", "parent-1").JobID, 3); jv.Status != "succeeded" {
		t.Fatalf("源任务应成功：%s", jv.Status)
	}
	vs := []BatchVariant{variant("va", "vb"), variant("vc", "vd")}
	q, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, vs)
	if err != nil {
		t.Fatal(err)
	}
	if q.MainDim != "garment" || q.RefsPerVariant != 2 {
		t.Fatalf("主替换项按逐套引用的图数：%+v", q)
	}
	bv, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, Variants: vs, ExpectedPoints: q.Total, IdempotencyKey: "b-occ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bv.Jobs) != 2 {
		t.Fatalf("批次：%+v", bv)
	}
	// 子方案保留逐套标注，且图换成了各自变体的图
	j, _ := e.store.GetJob(e.ctx, bv.Jobs[0].JobID)
	cp, _ := e.store.GetPlan(e.ctx, j.PlanID)
	var ann Annotations
	_ = json.Unmarshal(cp.Annotations, &ann)
	var refs []RefImage
	_ = json.Unmarshal(cp.Refs, &refs)
	if len(ann.Dims["garment"].Occurrences) != 2 || len(refs) != 2 || refs[0].Rid != testRid("va") || refs[1].Rid != testRid("vb") {
		t.Fatalf("子方案：%+v %+v", ann.Dims["garment"], refs)
	}
}

// ---- 任务来源 + 通知链接 + 待决策提醒（CONTRACT §O2）----

func agentCtx(conv string) context.Context {
	return WithOrigin(context.Background(), &Origin{Kind: OriginAgent, ConversationID: conv, TurnID: "t9"})
}

func TestOriginPersistedOnlyForAgentCalls(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"智能体", agentCtx("c-123"), true},
		{"没带 origin", context.Background(), false},
		{"kind 不是 agent", WithOrigin(context.Background(), &Origin{Kind: "web", ConversationID: "c-1"}), false},
		{"会话号为空", WithOrigin(context.Background(), &Origin{Kind: OriginAgent}), false},
		{"会话号含非法字符（进不了链接）", WithOrigin(context.Background(), &Origin{Kind: OriginAgent, ConversationID: "a&job=1"}), false},
	}
	for i, c := range cases {
		e := newEnv(t, false)
		e.ctx = c.ctx
		jv := e.confirm(e.plan(e.seedAnalysis(10)), "", fmt.Sprintf("k%d", i))
		j := e.job(jv.JobID)
		if (len(j.OriginJSON) > 0) != c.want {
			t.Errorf("%s：origin_json=%q 期望落库=%v", c.name, j.OriginJSON, c.want)
		}
	}
	e := newEnv(t, false)
	e.ctx = agentCtx("c-123")
	j := e.job(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID)
	if string(j.OriginJSON) != `{"kind":"agent","conversation_id":"c-123","turn_id":"t9"}` {
		t.Fatalf("落库内容：%s", j.OriginJSON)
	}
}

func TestOriginLooseIDsAndLink(t *testing.T) {
	var o Origin
	if err := json.Unmarshal([]byte(`{"kind":"agent","conversation_id":12345,"turn_id":"t"}`), &o); err != nil || o.ConversationID != "12345" {
		t.Fatalf("数字会话号应收下：%+v %v", o, err)
	}
	if got := JobLinkFor(&model.Job{ID: 9}); got != "/local-life/video-replicate?job=9" {
		t.Fatalf("无来源：%s", got)
	}
	if got := JobLinkFor(&model.Job{ID: 9, OriginJSON: json.RawMessage(`{"kind":"agent","conversation_id":"c-7"}`)}); got != "/agent?conversation=c-7&job=9" {
		t.Fatalf("智能体来源：%s", got)
	}
	if got := JobLinkFor(&model.Job{ID: 9, OriginJSON: json.RawMessage(`{"kind":"web","conversation_id":"c-7"}`)}); got != "/local-life/video-replicate?job=9" {
		t.Fatalf("非 agent 来源：%s", got)
	}
	if got := JobLinkFor(&model.Job{ID: 9, OriginJSON: json.RawMessage(`not json`)}); got != "/local-life/video-replicate?job=9" {
		t.Fatalf("坏数据回落：%s", got)
	}
}

// 来源只换通知链接：智能体确认的任务通知链回原对话，网页确认的维持任务页。
func TestNotifyLinkFollowsOrigin(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  context.Context
		want func(id int64) string
	}{
		{"agent", agentCtx("c-77"), func(id int64) string { return fmt.Sprintf("/agent?conversation=c-77&job=%d", id) }},
		{"web", context.Background(), JobLink},
	} {
		n := &fakeNotifier{}
		e := newOpsEnv(t, true, nil, func(*env) Option { return WithNotifier(n) })
		e.ctx = c.ctx
		jv := e.run(e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "", "k1").JobID, 3)
		fr := n.byKind(KindFirstReady)
		if len(fr) != 1 || fr[0].Link != c.want(jv.JobID) {
			t.Errorf("%s：链接 %+v", c.name, fr)
		}
	}
}

// reminderEnv 待继续的多段任务（第 1 段已做好，窗口 24 小时），返回环境与通知器。
func reminderEnv(t *testing.T, mut func(*Config)) (*env, *fakeNotifier, int64) {
	e, n, id, _ := reminderEnvPlan(t, mut)
	return e, n, id
}

func reminderEnvPlan(t *testing.T, mut func(*Config)) (*env, *fakeNotifier, int64, *PlanView) {
	n := &fakeNotifier{}
	e := newOpsEnv(t, true, mut, func(*env) Option { return WithNotifier(n) })
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if jv.Status != "waiting" {
		t.Fatalf("前置：应待继续：%s", jv.Status)
	}
	return e, n, jv.JobID, pv
}

func TestDecisionReminderTimingAndOnce(t *testing.T) {
	e, n, id := reminderEnv(t, nil)
	j := e.job(id)
	if j.DecisionRemindAt == nil || !j.DecisionRemindAt.Equal(j.ContinueDeadline.Add(-4*time.Hour)) {
		t.Fatalf("进入待继续应写提醒时刻 = 到期 - 4 小时：%v / %v", j.DecisionRemindAt, j.ContinueDeadline)
	}
	start := e.clock
	// 19 小时：还没到点，不提醒，也不被轮询取出
	e.clock = start.Add(19 * time.Hour)
	if ids, _ := e.store.DueJobs(e.ctx, 10); len(ids) != 0 {
		t.Fatalf("19 小时不该取出：%v", ids)
	}
	e.run(id, 1)
	if len(n.byKind(KindDecisionReminder)) != 0 {
		t.Fatal("19 小时不提醒")
	}
	// 20 小时：轮询取出 → 提醒一次
	e.clock = start.Add(20*time.Hour + time.Minute)
	if ids, _ := e.store.DueJobs(e.ctx, 10); len(ids) != 1 || ids[0] != id {
		t.Fatalf("20 小时应被轮询取出：%v", ids)
	}
	e.svc.Tick(e.ctx, 10)
	rm := n.byKind(KindDecisionReminder)
	if len(rm) != 1 || rm[0].Link != JobLink(id) || !strings.Contains(rm[0].Body, "做好 20 小时") || !strings.Contains(rm[0].Body, "4 小时后会自动结束") || !strings.Contains(rm[0].Body, "只保留第 1 段") {
		t.Fatalf("提醒内容：%+v", rm)
	}
	if e.job(id).DecisionRemindAt != nil {
		t.Fatal("提醒发出后应清掉提醒时刻")
	}
	// 之后再推进多少次都不重复
	e.clock = start.Add(22 * time.Hour)
	e.run(id, 3)
	e.svc.Tick(e.ctx, 10)
	if len(n.byKind(KindDecisionReminder)) != 1 {
		t.Fatal("每 job 只提醒一次")
	}
	// 到期后仍按原逻辑自动结束并通知
	e.clock = start.Add(25 * time.Hour)
	e.svc.Tick(e.ctx, 10)
	if jv, _ := e.svc.JobView(e.ctx, uid, id); jv.Status != "succeeded" {
		t.Fatalf("到期应自动结束：%s", jv.Status)
	}
	if len(n.byKind(KindDecisionReminder)) != 1 {
		t.Fatal("到期后不再提醒")
	}
}

// 提醒的幂等不依赖进程内缓存：换一个新 Service（同一份库 / 同一个通知器）再推进也不重复。
func TestDecisionReminderIdempotentAcrossRestart(t *testing.T) {
	e, n, id := reminderEnv(t, nil)
	e.clock = e.clock.Add(21 * time.Hour)
	e.run(id, 1)
	if len(n.byKind(KindDecisionReminder)) != 1 {
		t.Fatal("应提醒一次")
	}
	e.svc.sent = sentCache{} // 模拟重启：清进程内去重
	e.run(id, 2)
	if len(n.byKind(KindDecisionReminder)) != 1 {
		t.Fatal("库唯一键兜底：不重复")
	}
}

// 提前量可配（数据库 > yaml 的结果落到 Config）；0 / 关闭不提醒；用户已继续 / 已停止的不提醒。
func TestDecisionReminderConfigAndStates(t *testing.T) {
	// 提前量 6 小时：18 小时就提醒
	e, n, id := reminderEnv(t, func(c *Config) { c.DecisionReminderLead = 6 * time.Hour })
	if j := e.job(id); !j.DecisionRemindAt.Equal(j.ContinueDeadline.Add(-6 * time.Hour)) {
		t.Fatalf("提醒时刻：%v", j.DecisionRemindAt)
	}
	e.clock = e.clock.Add(18*time.Hour + time.Minute)
	e.run(id, 1)
	if rm := n.byKind(KindDecisionReminder); len(rm) != 1 || !strings.Contains(rm[0].Body, "6 小时后会自动结束") {
		t.Fatalf("6 小时提前量：%+v", rm)
	}
	// 关闭
	e, n, id = reminderEnv(t, func(c *Config) { c.DecisionReminderOff = true })
	if e.job(id).DecisionRemindAt != nil {
		t.Fatal("关闭时不写提醒时刻")
	}
	e.clock = e.clock.Add(21 * time.Hour)
	e.run(id, 1)
	if len(n.byKind(KindDecisionReminder)) != 0 {
		t.Fatal("关闭不提醒")
	}
	// 窗口不比提前量长：不提醒
	e, n, id = reminderEnv(t, func(c *Config) { c.ContinueWindow, c.DecisionReminderLead = 3*time.Hour, 4*time.Hour })
	e.clock = e.clock.Add(2 * time.Hour)
	e.run(id, 1)
	if len(n.byKind(KindDecisionReminder)) != 0 || e.job(id).DecisionRemindAt != nil {
		t.Fatal("窗口 ≤ 提前量不提醒")
	}
	// 用户在提醒之前已经继续：不提醒（任务已离开待继续）
	var pv *PlanView
	e, n, id, pv = reminderEnvPlan(t, nil)
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: id, PlanHash: pv.PlanHash, IdempotencyKey: "c1"}); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(21 * time.Hour)
	e.run(id, 3)
	if len(n.byKind(KindDecisionReminder)) != 0 {
		t.Fatal("已继续的任务不提醒")
	}
}
