package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
)

// ---- 替身 ----

type fakeVoiceCatalog struct {
	voices []VoiceInfo
}

func (c *fakeVoiceCatalog) ListVoices(context.Context) ([]VoiceInfo, error) { return c.voices, nil }
func (c *fakeVoiceCatalog) GetVoice(_ context.Context, id int64) (*VoiceInfo, error) {
	for _, v := range c.voices {
		if v.ID == id {
			v := v
			return &v, nil
		}
	}
	return nil, nil
}

// fakeVoiceStore 内存额度：按「任务占一次、只退一次」的口径，与 repo 实现同语义。
type fakeVoiceStore struct {
	mu   sync.Mutex
	used map[string]int
	use  map[int64]struct {
		key      string
		refunded bool
	}
}

func newVoiceStore() *fakeVoiceStore {
	return &fakeVoiceStore{used: map[string]int{}, use: map[int64]struct {
		key      string
		refunded bool
	}{}}
}

func (f *fakeVoiceStore) AcquireVoiceQuota(_ context.Context, userID, jobID int64, day string, total int) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("%d|%s", userID, day)
	if _, ok := f.use[jobID]; ok {
		return f.used[k], true, nil
	}
	if f.used[k] >= total {
		return f.used[k], false, nil
	}
	f.used[k]++
	f.use[jobID] = struct {
		key      string
		refunded bool
	}{key: k}
	return f.used[k], true, nil
}

func (f *fakeVoiceStore) ReleaseVoiceQuota(_ context.Context, jobID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.use[jobID]
	if !ok || u.refunded {
		return false, nil
	}
	u.refunded = true
	f.use[jobID] = u
	f.used[u.key]--
	return true, nil
}

func (f *fakeVoiceStore) VoiceQuotaUsed(_ context.Context, userID int64, day string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[fmt.Sprintf("%d|%s", userID, day)], nil
}

func (f *fakeVoiceStore) total(e *env) int {
	n, _ := f.VoiceQuotaUsed(e.ctx, uid, e.svc.previewDay())
	return n
}

// voiceEnv 装好合成器、音色目录与额度的场景；TTS 默认每句 2 秒。
func voiceEnv(t *testing.T, withCutter bool) (*env, *fakeComposer, *fakeVoiceStore) {
	e := newEnv(t, withCutter)
	fc := withComposer(e, "")
	vs := newVoiceStore()
	WithVoice(vs, &fakeVoiceCatalog{voices: []VoiceInfo{{ID: 5, Name: "小云", Voice: "xiaoyun", Gender: "female", Style: "亲切", PreviewURL: "https://oss/p.mp3"},
		{ID: 6, Name: "大川", Voice: "dachuan", Gender: "male", Style: "沉稳"}}})(e.svc)
	e.gw.ttsHook = func(req aigateway.CallRequest) (string, int64, bool) {
		return workRid("tts|" + req.Input.(map[string]any)["text"].(string)), 2000, false
	}
	return e, fc, vs
}

var voiceTestLines = []Sentence{{T0: 1, T1: 3, Text: "这双鞋穿着特别舒服"}, {T0: 13, T1: 16, Text: "今天下单立减五十元"}, {T0: 26, T1: 29, Text: "快来抢购吧"}}

// seedLinesAnalysis 带台词的分析（时长 d，分镜边界 cuts）。
func (e *env) seedLinesAnalysis(d float64, lines []Sentence, cuts ...float64) *model.Analysis {
	r := AnalysisResult{PromptVersion: AnalysisPromptVersion, DurationS: d, Width: 720, Height: 1280, HasAudio: true, AudioKind: "speech",
		Dims: map[string]Dim{}, DimOrder: DimKeys, Lines: lines}
	for _, k := range DimKeys {
		r.Dims[k] = Dim{Present: true, Desc: DimLabel[k] + "现状", Risk: "low"}
	}
	prev := 0.0
	for i, c := range append(cuts, d) {
		r.Sections = append(r.Sections, Section{Index: i + 1, T0: prev, T1: c, Desc: fmt.Sprintf("镜头%d", i+1)})
		prev = c
	}
	b, _ := json.Marshal(r)
	a, _, _ := e.store.InsertAnalysis(e.ctx, &model.Analysis{AnalysisNo: fmt.Sprintf("VRAv%v", d), UserID: uid, SourceKey: fmt.Sprintf("v%v", d),
		SourceURL: "https://x/v.mp4", SourceRid: ptr("src-rid"), Status: model.AnalysisSucceeded, Stage: model.StageDone, Result: b})
	return a
}

func (e *env) planVoice(a *model.Analysis, v *VoiceEdit) (*PlanView, error) {
	return e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5",
		Annotations: Annotations{Audio: AudioRevoice, Voice: v}})
}

// ---- 校验 ----

func TestVoiceValidationGaps(t *testing.T) {
	e, _, _ := voiceEnv(t, true)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	long := strings.Repeat("好", 11) // 第 1 句 2 秒 → 上限 10 字
	cases := []struct {
		name string
		ann  Annotations
		want string
	}{
		{"音色不存在", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 99}}, "VOICE_UNKNOWN"},
		{"没选音色", Annotations{Audio: AudioRevoice}, "VOICE_MISSING"},
		{"不是换声音却带了 voice", Annotations{Audio: AudioKeep, Dims: map[string]DimEdit{"product": {Mode: ModeDescribe, Note: "红色"}}, Voice: &VoiceEdit{VoiceID: 5}}, "VOICE_AUDIO_MODE"},
		{"句号越界", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 4, Text: "好"}}}}, "VOICE_LINE_INDEX"},
		{"句号 0", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 0, Text: "好"}}}}, "VOICE_LINE_INDEX"},
		{"句号重复", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 1, Text: "好"}, {Index: 1, Text: "不好"}}}}, "VOICE_LINE_DUPLICATE"},
		{"台词为空", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 1, Text: "  "}}}}, "VOICE_LINE_EMPTY"},
		{"超过字数上限", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 1, Text: long}}}}, "VOICE_LINE_TOO_LONG"},
		{"语速过快", Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Speed: 2}}, "VOICE_SPEED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", Annotations: c.ann})
			if got := gapCodesStr(err); got != c.want {
				t.Fatalf("gap = %q，期望 %q", got, c.want)
			}
		})
	}
	// 字数上限写在提示里
	_, err := e.planVoice(a, &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 1, Text: long}}})
	var ge *GapsError
	if !errors.As(err, &ge) || !strings.Contains(ge.Gaps[0].Message, "最多 10 个字") || !strings.Contains(ge.Gaps[0].Message, "第 1 句") {
		t.Fatalf("应写明上限：%v", err)
	}
	// 恰好等于上限放行
	if _, err := e.planVoice(a, &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 1, Text: strings.Repeat("好", 10)}}}); err != nil {
		t.Fatalf("上限内应放行：%v", err)
	}
}

// 没有台词的视频：VOICE_NO_LINES，并说明视频里没有可换声音的说话。
func TestVoiceNoLinesGap(t *testing.T) {
	e, _, _ := voiceEnv(t, false)
	a := e.seedLinesAnalysis(10, nil)
	_, err := e.planVoice(a, &VoiceEdit{VoiceID: 5})
	var ge *GapsError
	if !errors.As(err, &ge) || ge.Gaps[0].Code != "VOICE_NO_LINES" || !strings.Contains(ge.Gaps[0].Message, "没有") {
		t.Fatalf("%v", err)
	}
}

// 换声音没开放（没装配 / 运行时关闭）：方案拒收 VOICE_UNAVAILABLE；音色列表回 7003。
func TestVoiceUnavailableWhenOff(t *testing.T) {
	e, _, _ := voiceEnv(t, false)
	e.svc.cfg.VoiceOff = true
	a := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	if _, err := e.planVoice(a, &VoiceEdit{VoiceID: 5}); gapCodesStr(err) != "VOICE_UNAVAILABLE" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.ListVoices(e.ctx, VoiceListReq{}); err == nil {
		t.Fatal("关闭时列音色应报错")
	}
}

// 不用换声音时 annotations 的 JSON 与 plan_hash 与旧版逐字一致；用了换声音才变。
func TestVoiceAbsentKeepsJSONAndPlanHash(t *testing.T) {
	b, _ := json.Marshal(Annotations{Dims: map[string]DimEdit{}, Audio: AudioRegenerate})
	if string(b) != `{"dims":{},"audio":"regenerate"}` {
		t.Fatalf("未用换声音时 JSON 必须不变：%s", b)
	}
	e, _, _ := voiceEnv(t, true)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	p1 := e.plan(a)
	p2 := e.plan(a)
	if p1.PlanHash != p2.PlanHash {
		t.Fatal("同输入同指纹")
	}
	if strings.Contains(string(mustJSON(p1.Annotations)), "voice") {
		t.Fatalf("方案标注里不应出现 voice：%s", mustJSON(p1.Annotations))
	}
	pv, err := e.planVoice(a, &VoiceEdit{VoiceID: 5})
	if err != nil || pv.PlanHash == p1.PlanHash {
		t.Fatalf("换声音是另一个方案：%v", err)
	}
	pv2, _ := e.planVoice(a, &VoiceEdit{VoiceID: 5, Speed: 1})
	if pv2.PlanHash != pv.PlanHash {
		t.Fatal("语速 1.0 等于缺省，指纹应一致")
	}
	pv3, _ := e.planVoice(a, &VoiceEdit{VoiceID: 6})
	if pv3.PlanHash == pv.PlanHash {
		t.Fatal("换音色指纹应变")
	}
}

// 改成与原句一样 = 没改；逐句按 index 升序规范化。
func TestVoiceLinesNormalized(t *testing.T) {
	e, _, _ := voiceEnv(t, true)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	pv, err := e.planVoice(a, &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 3, Text: " 快来买吧 "}, {Index: 1, Text: "这双鞋穿着特别舒服"}, {Index: 2, Text: "全场五折"}}})
	if err != nil {
		t.Fatal(err)
	}
	l := pv.Annotations.Voice.Lines
	if len(l) != 2 || l[0].Index != 2 || l[1].Index != 3 || l[1].Text != "快来买吧" {
		t.Fatalf("%+v", l)
	}
}

// 方案卡：配音设置大白话、口型提示、报价与普通方案相同（免费）。
func TestVoicePlanViewAndQuoteUnchanged(t *testing.T) {
	e, _, _ := voiceEnv(t, true)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	base := e.plan(a)
	pv, err := e.planVoice(a, &VoiceEdit{VoiceID: 5, Speed: 1.1, Lines: []VoiceLineEdit{{Index: 2, Text: "全场五折"}}})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Cost.Total != base.Cost.Total || pv.StageQuotes != base.StageQuotes {
		t.Fatalf("换声音不改报价：%d vs %d", pv.Cost.Total, base.Cost.Total)
	}
	var voice, lines *DimLine
	for i := range pv.Changes {
		switch pv.Changes[i].Key {
		case "voice":
			voice = &pv.Changes[i]
		case "voiceLines":
			lines = &pv.Changes[i]
		}
	}
	if voice == nil || !strings.Contains(voice.Note, "小云") || !strings.Contains(voice.Note, "1.1") || lines == nil || !strings.Contains(lines.Note, "第 2 句") {
		t.Fatalf("方案卡没写清配音设置：%+v", pv.Changes)
	}
	found := false
	for _, r := range pv.Risks {
		found = found || r == "换声音后口型可能对不上"
	}
	if !found || !strings.Contains(pv.Overview, "换声音") {
		t.Fatalf("口型提示 / 概述：%v %s", pv.Risks, pv.Overview)
	}
	// 没改台词：保持不变里说沿用
	pv2, _ := e.planVoice(a, &VoiceEdit{VoiceID: 5})
	kept := false
	for _, k := range pv2.Kept {
		kept = kept || (k.Key == "voiceLines" && strings.Contains(k.Note, "沿用原片"))
	}
	if !kept {
		t.Fatalf("Kept：%+v", pv2.Kept)
	}
}

// 改写的台词进文本审核输入；原句不审。
func TestVoiceEditedTextsAreModerated(t *testing.T) {
	ann := Annotations{Audio: AudioRevoice, Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 2, Text: "全场五折"}, {Index: 3, Text: " 快来买 "}}}}
	if got := voiceTexts(ann); len(got) != 2 || got[0] != "全场五折" || got[1] != "快来买" {
		t.Fatalf("%v", got)
	}
	e, _, _ := voiceEnv(t, true)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	pv, err := e.planVoice(a, &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 2, Text: "全场五折"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	in, err := e.svc.collectModInputs(e.ctx, p)
	if err != nil || len(in.texts) != 1 || in.texts[0] != "全场五折" {
		t.Fatalf("审核输入：%v %v", in.texts, err)
	}
}

func TestListVoicesFilter(t *testing.T) {
	e, _, _ := voiceEnv(t, false)
	all, err := e.svc.ListVoices(e.ctx, VoiceListReq{})
	if err != nil || len(all.Voices) != 2 || all.Voices[0].SampleURL != "https://oss/p.mp3" {
		t.Fatalf("%+v %v", all, err)
	}
	if b, _ := json.Marshal(all); strings.Contains(string(b), "xiaoyun") {
		t.Fatalf("不应暴露网关音色参数：%s", b)
	}
	m, _ := e.svc.ListVoices(e.ctx, VoiceListReq{Gender: "男"})
	if len(m.Voices) != 1 || m.Voices[0].VoiceID != 6 {
		t.Fatalf("%+v", m)
	}
	s, _ := e.svc.ListVoices(e.ctx, VoiceListReq{Gender: "female", Style: "亲切"})
	if len(s.Voices) != 1 || s.Voices[0].VoiceID != 5 {
		t.Fatalf("%+v", s)
	}
	none, _ := e.svc.ListVoices(e.ctx, VoiceListReq{Style: "不存在"})
	if none.Voices == nil || len(none.Voices) != 0 {
		t.Fatalf("空结果应是空数组：%+v", none)
	}
}

// ---- 时间轴映射 ----

func TestVoicePlacements(t *testing.T) {
	seg := func(i int, t0, t1 float64) JobSegment {
		var s JobSegment
		s.Index, s.T0, s.T1 = i, t0, t1
		return s
	}
	lines := []VoiceLine{{Index: 1, T0: 1, T1: 3}, {Index: 2, T0: 13, T1: 16}, {Index: 3, T0: 26, T1: 29}}
	outs := []segOutcome{{seg: seg(1, 0, 12), rid: "a"}, {seg: seg(2, 12, 25), failed: true}, {seg: seg(3, 25, 32), rid: "c"}}
	ps, total := voicePlacements(outs, lines)
	if total != 32 || len(ps) != 3 || ps[0].Start != 1 || ps[1].Start != 13 || ps[2].Start != 26 {
		t.Fatalf("占位段照常占时间：total=%v %+v", total, ps)
	}
	// 时间窗 = 到下一句起点，但不超过原句长度 ×1.25 + 0.5
	if ps[0].Slot != 3.0 || ps[1].Slot != 4.25 || ps[2].Slot != 4.25 {
		t.Fatalf("时间窗：%v %v %v", ps[0].Slot, ps[1].Slot, ps[2].Slot)
	}
	// 中间的段没做（用户不继续）：成片里它不占时间，后面的句子前移；落在没做的段里的句子不配
	outs = []segOutcome{{seg: seg(1, 0, 12), rid: "a"}, {seg: seg(2, 12, 25), omitted: true}, {seg: seg(3, 25, 32), rid: "c"}}
	ps, total = voicePlacements(outs, lines)
	if total != 19 || len(ps) != 2 || ps[0].Index != 1 || ps[1].Index != 3 || ps[1].Start != 13 {
		t.Fatalf("略过的段：total=%v %+v", total, ps)
	}
	// 相邻两句挨得很近：前一句的窗被下一句起点截住
	ps, _ = voicePlacements([]segOutcome{{seg: seg(1, 0, 10), rid: "a"}}, []VoiceLine{{Index: 1, T0: 1, T1: 4}, {Index: 2, T0: 3.5, T1: 6}})
	if ps[0].Slot != 2.5 {
		t.Fatalf("窗应到下一句起点：%v", ps[0].Slot)
	}
}

// ---- 合成：铺位、提速、截断 ----

func (e *env) voiceJobAll(t *testing.T, a *model.Analysis, v *VoiceEdit, key string) *JobView {
	t.Helper()
	pv, err := e.planVoice(a, v)
	if err != nil {
		t.Fatalf("方案：%v", err)
	}
	return e.confirm(pv, "all", key)
}

func ttsInputs(e *env) []map[string]any {
	var out []map[string]any
	e.gw.mu.Lock()
	defer e.gw.mu.Unlock()
	for _, k := range e.gw.submits {
		if r := e.gw.reqs[k]; r.Operation == "tts.synthesize" {
			out = append(out, r.Input.(map[string]any))
		}
	}
	return out
}

func TestRevoiceSynthesisPlacesClipsAndReplacesAudio(t *testing.T) {
	e, fc, vs := voiceEnv(t, true)
	ff := withFunnel(e)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	jv := e.voiceJobAll(t, a, &VoiceEdit{VoiceID: 5, Speed: 1.1, Lines: []VoiceLineEdit{{Index: 2, Text: "全场五折"}}}, "k1")
	if vs.total(e) != 1 {
		t.Fatalf("确认时应占一条额度：%d", vs.total(e))
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "succeeded" || jv.Result == nil || jv.Result.Status != ResultReady {
		t.Fatalf("%s %+v", jv.Status, jv.Result)
	}
	if len(fc.drops) != 1 || fc.drops[0] {
		t.Fatalf("换声音拼接不去声：%v", fc.drops)
	}
	if len(fc.timelines) != 1 || len(fc.timelines[0].clips) != 3 || fc.timelines[0].total != 32 {
		t.Fatalf("音轨：%+v", fc.timelines)
	}
	c := fc.timelines[0].clips
	if c[0].Start != 1 || c[1].Start != 13 || c[2].Start != 26 || c[0].MaxSeconds != 3 || c[1].MaxSeconds != 4.25 {
		t.Fatalf("铺位 / 时间窗：%+v", c)
	}
	if len(fc.muxes) != 1 || fc.muxes[0].source != "track-1" || fc.muxes[0].video != "final-1" ||
		len(fc.muxes[0].ranges) != 1 || fc.muxes[0].ranges[0].Start != 0 || fc.muxes[0].ranges[0].End != 32 {
		t.Fatalf("替换音轨：%+v", fc.muxes)
	}
	if jv.Result.VideoRid != "mux(final-1)" || jv.Result.Audio != AudioResultRevoiced || jv.Result.AudioNote != "" {
		t.Fatalf("成片：%+v", jv.Result)
	}
	// 请求内容：改过的台词、原句、音色参数、语速
	texts := map[string]bool{}
	for _, in := range ttsInputs(e) {
		texts[in["text"].(string)] = true
		if in["voice"] != "xiaoyun" || in["format"] != "mp3" || in["speed"] != 1.1 {
			t.Fatalf("tts 入参：%v", in)
		}
	}
	if !texts["全场五折"] || !texts["这双鞋穿着特别舒服"] || !texts["快来抢购吧"] || texts["今天下单立减五十元"] {
		t.Fatalf("合成的台词：%v", texts)
	}
	for _, k := range e.gw.submits {
		if r := e.gw.reqs[k]; r.Operation == "tts.synthesize" && r.ModelGroup != "tts.minimax-speech-2.8-hd" {
			t.Fatalf("模型组：%s", r.ModelGroup)
		}
	}
	if vs.total(e) != 1 {
		t.Fatal("成功不退额度")
	}
	if n := ff.count(StageRevoice); n != 1 {
		t.Fatalf("漏斗 revoice=%d", n)
	}
	e.assertMoney(jv.JobID)
	// 同一个任务再合成一次（成片依据没变不会重做；这里直接重放合成）：TTS 幂等键相同，网关不重复提交
	before := len(ttsInputs(e))
	j, _ := e.store.GetJob(e.ctx, jv.JobID)
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	var doc ResultDoc
	outs := e.svc.outcomes(j, &in, e.steps(jv.JobID))
	e.svc.revoiceAudio(e.ctx, j, &in, outs, "final-1", func(s string) string { return "x/" + s }, &doc)
	if len(ttsInputs(e)) != before {
		t.Fatalf("重放合成不应再向网关提交新的调用：%d → %d", before, len(ttsInputs(e)))
	}
}

// 句子比时间窗长：语速上调至多 1.3 倍重做一次；仍放不下就保留并截尾，记 warning。
func TestRevoiceSpeedUpThenTruncate(t *testing.T) {
	e, fc, _ := voiceEnv(t, true)
	e.gw.ttsHook = func(req aigateway.CallRequest) (string, int64, bool) {
		in := req.Input.(map[string]any)
		speed, _ := in["speed"].(float64)
		if speed == 0 {
			speed = 1
		}
		base := map[string]float64{"这双鞋穿着特别舒服": 4.0, "今天下单立减五十元": 5.0}[in["text"].(string)]
		if base == 0 {
			base = 2
		}
		return workRid(fmt.Sprintf("tts|%s|%v", in["text"], speed)), int64(base / speed * 1000), false
	}
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	jv := e.run(e.voiceJobAll(t, a, &VoiceEdit{VoiceID: 5}, "k1").JobID, 6)
	ins := ttsInputs(e)
	speeds := map[string][]float64{}
	for _, in := range ins {
		s, _ := in["speed"].(float64)
		speeds[in["text"].(string)] = append(speeds[in["text"].(string)], s)
	}
	// 第 1 句 4.0s / 窗 3.0s：比值 1.37 → 上限 1.3 倍，重做后 3.08s 仍超 → 截尾；第 2 句 5.0s / 窗 4.25s：1.21 倍重做后放得下；第 3 句不动
	l1 := speeds["这双鞋穿着特别舒服"]
	l2 := speeds["今天下单立减五十元"]
	if len(l1) != 2 || l1[0] != 0 || l1[1] != 1.3 {
		t.Fatalf("第 1 句应先原速再 1.3 倍：%v", l1)
	}
	if len(l2) != 2 || l2[1] < 1.2 || l2[1] > 1.22 {
		t.Fatalf("第 2 句应提到约 1.21 倍：%v", l2)
	}
	if len(speeds["快来抢购吧"]) != 1 {
		t.Fatalf("放得下的句子不重做：%v", speeds["快来抢购吧"])
	}
	d := resultDoc(t, e, jv.JobID)
	if len(d.VoiceTrimmed) != 1 || d.VoiceTrimmed[0] != 1 || !contains(d.Warnings, "VOICE_LINE_TRUNCATED:1") {
		t.Fatalf("应只截第 1 句：%+v", d)
	}
	if jv.Result.Audio != AudioResultRevoiced || !strings.Contains(jv.Result.AudioNote, "第 1 句") {
		t.Fatalf("应提示被截短：%+v", jv.Result)
	}
	if c := fc.timelines[0].clips; c[0].MaxSeconds != 3 {
		t.Fatalf("时间线按窗截断：%+v", c)
	}
}

// ---- 回落 ----

func assertRevoiceFellBack(t *testing.T, e *env, vs *fakeVoiceStore, jv *JobView) {
	t.Helper()
	if jv.Status != "succeeded" || jv.Result == nil || jv.Result.Status != ResultReady || jv.Result.VideoRid != "final-1" {
		t.Fatalf("失败应回落到未换声的成片、不拦交付：%s %+v", jv.Status, jv.Result)
	}
	if jv.Result.Audio != AudioResultFailed || jv.Result.AudioNote != audioFailedNote {
		t.Fatalf("应告诉用户没换成：%+v", jv.Result)
	}
	if vs.total(e) != 0 {
		t.Fatalf("合成失败应退回额度：%d", vs.total(e))
	}
	e.assertMoney(jv.JobID)
}

func TestRevoiceFallbacks(t *testing.T) {
	a := func(e *env) *model.Analysis { return e.seedLinesAnalysis(32, voiceTestLines, 12, 25) }
	t.Run("TTS 失败", func(t *testing.T) {
		e, _, vs := voiceEnv(t, true)
		e.gw.ttsHook = func(aigateway.CallRequest) (string, int64, bool) { return "", 0, true }
		jv := e.run(e.voiceJobAll(t, a(e), &VoiceEdit{VoiceID: 5}, "k1").JobID, 6)
		assertRevoiceFellBack(t, e, vs, jv)
	})
	t.Run("只有一句失败也整体回落", func(t *testing.T) {
		e, fc, vs := voiceEnv(t, true)
		e.gw.ttsHook = func(req aigateway.CallRequest) (string, int64, bool) {
			return workRid("x|" + req.Input.(map[string]any)["text"].(string)), 2000, req.Input.(map[string]any)["text"] == "快来抢购吧"
		}
		jv := e.run(e.voiceJobAll(t, a(e), &VoiceEdit{VoiceID: 5}, "k1").JobID, 6)
		assertRevoiceFellBack(t, e, vs, jv)
		if len(fc.timelines) != 0 {
			t.Fatal("不应铺半条音轨")
		}
	})
	t.Run("铺音轨失败", func(t *testing.T) {
		e, fc, vs := voiceEnv(t, true)
		fc.timelineErr = errors.New("media 503")
		assertRevoiceFellBack(t, e, vs, e.run(e.voiceJobAll(t, a(e), &VoiceEdit{VoiceID: 5}, "k1").JobID, 6))
	})
	t.Run("替换音轨失败", func(t *testing.T) {
		e, fc, vs := voiceEnv(t, true)
		fc.muxErr = errors.New("media 503")
		assertRevoiceFellBack(t, e, vs, e.run(e.voiceJobAll(t, a(e), &VoiceEdit{VoiceID: 5}, "k1").JobID, 6))
	})
	t.Run("网关超时", func(t *testing.T) {
		e, _, vs := voiceEnv(t, true)
		e.gw.submitErr = errors.New("gw down")
		// 出片本身也要网关，所以只在出片后才让网关坏：先推进出片，再注入故障
		e.gw.submitErr = nil
		jv := e.voiceJobAll(t, a(e), &VoiceEdit{VoiceID: 5}, "k1")
		e.svc.cfg.VoiceWait = time.Millisecond
		e.gw.ttsHook = func(aigateway.CallRequest) (string, int64, bool) { return "", 0, true }
		assertRevoiceFellBack(t, e, vs, e.run(jv.JobID, 6))
	})
	t.Run("合成器没实现音轨铺设", func(t *testing.T) {
		e, fc, vs := voiceEnv(t, true)
		WithComposer(noMuxComposer{fc})(e.svc)
		assertRevoiceFellBack(t, e, vs, e.run(e.voiceJobAll(t, a(e), &VoiceEdit{VoiceID: 5}, "k1").JobID, 6))
	})
}

// ---- 额度 ----

func TestRevoiceQuotaConsumeReplayAndExhaust(t *testing.T) {
	e, _, vs := voiceEnv(t, false)
	e.svc.cfg.VoiceDailyQuota = 2
	a1 := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	a2 := e.seedLinesAnalysis(11, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	a3 := e.seedLinesAnalysis(12, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	j1 := e.voiceJobAll(t, a1, &VoiceEdit{VoiceID: 5}, "k1")
	pv1, _ := e.planVoice(a1, &VoiceEdit{VoiceID: 5})
	// 同键重放：不重复占
	if again, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv1.PlanNo, PlanHash: pv1.PlanHash, IdempotencyKey: "k1"}); err != nil || again.JobID != j1.JobID {
		t.Fatalf("重放：%v", err)
	}
	if vs.total(e) != 1 {
		t.Fatalf("重放不重复占：%d", vs.total(e))
	}
	e.voiceJobAll(t, a2, &VoiceEdit{VoiceID: 5}, "k2")
	if vs.total(e) != 2 {
		t.Fatalf("%d", vs.total(e))
	}
	// 第 3 条：确认时 VOICE_QUOTA_EXHAUSTED，没建任务、没冻点数、方案仍可确认
	pv3, _ := e.planVoice(a3, &VoiceEdit{VoiceID: 5})
	holds := e.ledger.holds
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv3.PlanNo, PlanHash: pv3.PlanHash, IdempotencyKey: "k3"})
	if reasonOf(err) != ReasonVoiceQuotaExhausted {
		t.Fatalf("应 VOICE_QUOTA_EXHAUSTED：%v", err)
	}
	if e.ledger.holds != holds {
		t.Fatal("额度用完不得冻点数")
	}
	if j, _ := e.store.GetJobByRequest(e.ctx, uid, "k3"); j != nil {
		t.Fatal("不应建任务")
	}
	if p, _ := e.store.GetPlanByNo(e.ctx, uid, pv3.PlanNo); p.Status != model.PlanReady {
		t.Fatalf("方案应仍可确认：%s", p.Status)
	}
	// 非换声音的方案不受额度影响
	e.confirm(e.plan(e.seedAnalysis(13)), "", "k4")
}

// 并发占满：precheck 通过但原子占用失败 → 整个确认回滚（没冻点数、方案放回）。
func TestRevoiceQuotaAcquireRaceRollsBack(t *testing.T) {
	e, _, vs := voiceEnv(t, false)
	e.svc.cfg.VoiceDailyQuota = 1
	a := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	pv, _ := e.planVoice(a, &VoiceEdit{VoiceID: 5})
	// 预检看到 0，占用前别的请求把额度占满
	racing := &raceStore{fakeVoiceStore: vs, fill: func() { _, _, _ = vs.AcquireVoiceQuota(e.ctx, uid, 9999, e.svc.previewDay(), 1) }}
	e.svc.voices = racing
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonVoiceQuotaExhausted {
		t.Fatalf("%v", err)
	}
	if e.ledger.holds != 0 {
		t.Fatalf("不得冻点数：%d", e.ledger.holds)
	}
	if p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo); p.Status != model.PlanReady {
		t.Fatalf("方案应放回：%s", p.Status)
	}
	if j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1"); j == nil || j.Status != model.JobFailed {
		t.Fatalf("任务应置失败：%+v", j)
	}
}

// raceStore 第一次查额度（预检）之后、占用之前，让别的请求把额度占满。
type raceStore struct {
	*fakeVoiceStore
	fill func()
	once sync.Once
}

func (r *raceStore) VoiceQuotaUsed(ctx context.Context, userID int64, day string) (int, error) {
	n, err := r.fakeVoiceStore.VoiceQuotaUsed(ctx, userID, day)
	r.once.Do(r.fill)
	return n, err
}

// 任务整体没成功（所有段失败）：额度退回，且只退一次。
func TestRevoiceQuotaRefundedWhenJobFails(t *testing.T) {
	e, _, vs := voiceEnv(t, false)
	a := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	jv := e.voiceJobAll(t, a, &VoiceEdit{VoiceID: 5}, "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "failed" {
		t.Fatalf("%s", jv.Status)
	}
	if vs.total(e) != 0 {
		t.Fatalf("任务失败应退回额度：%d", vs.total(e))
	}
	e.run(jv.JobID, 3) // 再推进也不会多退
	if vs.total(e) != 0 {
		t.Fatalf("只退一次：%d", vs.total(e))
	}
	if len(ttsInputs(e)) != 0 {
		t.Fatal("没有成片就不合成配音")
	}
}

// 确认中途预留点数失败（余额不足）：额度退回。
func TestRevoiceQuotaRefundedWhenHoldFails(t *testing.T) {
	e, _, vs := voiceEnv(t, false)
	e.ledger.balance = 0
	a := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	pv, _ := e.planVoice(a, &VoiceEdit{VoiceID: 5})
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"}); err == nil {
		t.Fatal("余额不足应失败")
	}
	if vs.total(e) != 0 {
		t.Fatalf("没提交成功不占额度：%d", vs.total(e))
	}
}

// 音色在方案之后下线：确认时拒（VOICE_UNAVAILABLE），没动钱、没占额度。
func TestRevoiceVoiceOfflineAtConfirm(t *testing.T) {
	e, _, vs := voiceEnv(t, false)
	a := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	pv, _ := e.planVoice(a, &VoiceEdit{VoiceID: 5})
	e.svc.voiceCat = &fakeVoiceCatalog{}
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != reasonVoiceUnavailable || vs.total(e) != 0 || e.ledger.holds != 0 {
		t.Fatalf("%v used=%d holds=%d", err, vs.total(e), e.ledger.holds)
	}
}

// 批量：每条继承音色设置，各占一条额度；额度放不下整批不做。
func TestRevoiceBatchInheritsAndChecksQuota(t *testing.T) {
	e, _, vs := voiceEnv(t, false)
	e.svc.cfg.VoiceDailyQuota = 2
	WithBatch(&memBatch{e: e})(e.svc)
	a := e.seedLinesAnalysis(10, []Sentence{{T0: 1, T1: 3, Text: "你好呀朋友"}})
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}, Audio: AudioRevoice,
			Voice: &VoiceEdit{VoiceID: 5, Lines: []VoiceLineEdit{{Index: 1, Text: "新台词"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	jv := e.run(e.confirm(pv, "", "src").JobID, 4)
	if jv.Status != "succeeded" {
		t.Fatalf("%s", jv.Status)
	}
	if vs.total(e) != 1 {
		t.Fatalf("%d", vs.total(e))
	}
	variants := func(n int) []BatchVariant {
		out := make([]BatchVariant, n)
		for i := range out {
			out[i] = BatchVariant{RefImages: []string{testRef}}
		}
		return out
	}
	bp, err := e.svc.loadBatchPlan(e.ctx, uid, pv.PlanNo)
	if err != nil {
		t.Fatal(err)
	}
	per, _, _ := e.svc.perJobPoints(e.ctx, bp)
	// 还剩 1 条额度，批量要 2 条 → 整批不做
	_, err = e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, Variants: variants(2), ExpectedPoints: per * 2, IdempotencyKey: "b1"})
	if reasonOf(err) != ReasonVoiceQuotaExhausted {
		t.Fatalf("应 VOICE_QUOTA_EXHAUSTED：%v", err)
	}
	if vs.total(e) != 1 {
		t.Fatalf("整批不做不占额度：%d", vs.total(e))
	}
	// 1 条：继承音色设置
	bv, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, Variants: variants(1), ExpectedPoints: per, IdempotencyKey: "b2"})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.store.GetJob(e.ctx, bv.Jobs[0].JobID)
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	if in.Audio != AudioRevoice || in.Voice == nil || in.Voice.VoiceID != 5 || in.Voice.Lines[0].Text != "新台词" || vs.total(e) != 2 {
		t.Fatalf("变体应继承音色设置并各占额度：%+v used=%d", in.Voice, vs.total(e))
	}
}
