package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/analysisfix"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

// ---- ETA 历史校准 ----

type fakeEtaSource struct {
	samples []EtaSample
	err     error
	calls   int
	since   time.Time
	limit   int
}

func (f *fakeEtaSource) EtaSamples(_ context.Context, since time.Time, limit int) ([]EtaSample, error) {
	f.calls++
	f.since, f.limit = since, limit
	return f.samples, f.err
}

// samplesOf n 条样本，耗时 100、110、120…（秒）。
func samplesOf(group string, segTotal, gen, n int) []EtaSample {
	out := make([]EtaSample, n)
	for i := range out {
		out[i] = EtaSample{ModelGroup: group, SegTotal: segTotal, GenSeconds: gen, Seconds: float64(100 + 10*i)}
	}
	return out
}

// withEta 用给定的历史校准重建内核（其余同 newEnv）。
func (e *env) withEta(cal *EtaCalibrator, mut func(*Config)) {
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	if mut != nil {
		mut(&cfg)
	}
	opts := []Option{WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }), WithOwner("test-owner"),
		WithURLOwner(testOwner), WithSigner(e.signer), WithEtaCalibrator(cal)}
	if e.cutter != nil {
		opts = append(opts, WithCutter(e.cutter))
	}
	e.svc = New(cfg, e.store, e.ledger, opts...)
}

func TestEtaBuckets(t *testing.T) {
	for n, want := range map[int]string{0: "1", 1: "1", 2: "2", 3: "3", 4: "4+", 6: "4+"} {
		if got := EtaSegBucket(n); got != want {
			t.Fatalf("段数 %d → %s，应为 %s", n, got, want)
		}
	}
	for s, want := range map[int]string{4: "0-5", 5: "0-5", 6: "6-10", 10: "6-10", 11: "11-15", 15: "11-15", 16: "16+", 30: "16+"} {
		if got := EtaDurBucket(s); got != want {
			t.Fatalf("时长 %d → %s，应为 %s", s, got, want)
		}
	}
}

func TestBuildEtaTablePercentiles(t *testing.T) {
	tbl := buildEtaTable(append(samplesOf("vid.a", 3, 14, 20), EtaSample{ModelGroup: "", Seconds: 9}, EtaSample{ModelGroup: "vid.a", Seconds: 0}))
	st := tbl[etaKey{"vid.a", "3", "11-15"}]
	// 100..290 共 20 个：P50 = 第 10 个 = 190，P90 = 第 18 个 = 270；无模型组 / 0 秒的样本丢弃
	if st.N != 20 || st.P50 != 190 || st.P90 != 270 {
		t.Fatalf("精确桶：%+v", st)
	}
	if c := tbl[etaKey{"vid.a", etaAnySegs, "11-15"}]; c.N != 20 {
		t.Fatalf("粗桶也应计入：%+v", c)
	}
}

func TestPlanEtaCalibratedFromHistory(t *testing.T) {
	e := newEnv(t, true)
	src := &fakeEtaSource{samples: samplesOf("vid.seedance-2.5", 3, 14, 20)}
	cal := NewEtaCalibrator(src, EtaCalibration{})
	if err := cal.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if src.limit != 5000 || time.Since(src.since) < 29*24*time.Hour {
		t.Fatalf("默认窗口 30 天、最多 5000 段：%v %d", src.since, src.limit)
	}
	e.withEta(cal, nil)
	p := e.plan(e.seedAnalysis(32, 12, 25)) // 3 段 12/14/8 秒 → 段数桶 3、时长桶 11-15
	if p.ETA.Prior || p.ETA.Total.LowSec != 190 || p.ETA.Total.HighSec != 270 || p.ETA.Total.Text != "预计 4–5 分钟" {
		t.Fatalf("应按历史 P50/P90：%+v", p.ETA)
	}
	if p.ETA.First == nil || p.ETA.Rest == nil || p.ETA.Rest.HighSec != 270 {
		t.Fatalf("多段应有 first/rest（3 段同时在途 = 1 轮）：%+v", p.ETA)
	}
	// 同时在途 1 段：3 轮
	e.withEta(cal, func(c *Config) { c.MaxInFlight = 1 })
	p = e.plan(e.seedAnalysis(32, 12, 25))
	if p.ETA.Total.LowSec != 570 || p.ETA.Total.HighSec != 810 || p.ETA.Rest.LowSec != 380 {
		t.Fatalf("R = ceil(N/在途)：%+v", p.ETA)
	}
}

func TestPlanEtaCoarseBucketFallback(t *testing.T) {
	e := newEnv(t, true)
	// 只有单段 job 的样本（段数桶 1），同模型同时长桶：3 段方案的精确桶没有样本 → 用不分段数的粗桶
	cal := NewEtaCalibrator(&fakeEtaSource{samples: samplesOf("vid.seedance-2.5", 1, 13, 25)}, EtaCalibration{})
	_ = cal.Refresh(e.ctx)
	e.withEta(cal, nil)
	p := e.plan(e.seedAnalysis(32, 12, 25))
	if p.ETA.Prior || p.ETA.Total.LowSec != 220 {
		t.Fatalf("应回落粗桶（25 个样本 P50 = 第 13 个 = 220）：%+v", p.ETA)
	}
}

func TestPlanEtaFallsBackToPrior(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedAnalysis(32, 12, 25)
	cases := map[string]*fakeEtaSource{
		"样本不足": {samples: samplesOf("vid.seedance-2.5", 3, 14, 19)},
		"别的模型": {samples: samplesOf("vid.wan-3.0", 3, 14, 50)},
		"别的时长": {samples: samplesOf("vid.seedance-2.5", 3, 25, 50)},
		"查库失败": {err: errors.New("db down")},
	}
	for name, src := range cases {
		cal := NewEtaCalibrator(src, EtaCalibration{})
		_ = cal.Refresh(e.ctx)
		e.withEta(cal, nil)
		p := e.plan(a)
		if !p.ETA.Prior || p.ETA.Total.LowSec != 240 || p.ETA.Total.HighSec != 360 || p.ETA.Total.Text != "预计 4–6 分钟" {
			t.Fatalf("%s：应回落先验并保留 prior=true：%+v", name, p.ETA)
		}
	}
	// 没装配校准：先验
	e.withEta(nil, nil)
	if p := e.plan(a); !p.ETA.Prior {
		t.Fatalf("未装配应为先验：%+v", p.ETA)
	}
}

func TestEtaCalibratorKeepsLastOnError(t *testing.T) {
	src := &fakeEtaSource{samples: samplesOf("vid.x", 2, 8, 20)}
	cal := NewEtaCalibrator(src, EtaCalibration{})
	if err := cal.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.err, src.samples = errors.New("timeout"), nil
	if err := cal.Refresh(context.Background()); err == nil {
		t.Fatal("应返回错误")
	}
	if st, ok := cal.lookup("vid.x", 2, 8); !ok || st.N != 20 {
		t.Fatalf("重算失败应沿用上一次：%+v %v", st, ok)
	}
	if calls := src.calls; calls != 2 {
		t.Fatalf("刷新间隔内 lookup 不应再查库：%d", calls)
	}
}

// ---- 发送前二次确认阈值 ----

func TestConfirmThresholdRequiresExpectedPoints(t *testing.T) {
	e := newEnv(t, true)
	e.withEta(nil, func(c *Config) { c.ConfirmThresholdPoints = 300 })
	p := e.plan(e.seedAnalysis(32, 12, 25)) // 方案总价 340 > 300；先做第 1 段 = 120
	for _, tc := range []struct {
		pts  int64
		want string
	}{{0, "这次要预留 120 点"}, {130, "点数有变化：130 → 120 点"}, {340, "点数有变化：340 → 120 点"}} {
		_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p.PlanNo, PlanHash: p.PlanHash, Stage: "first",
			IdempotencyKey: fmt.Sprintf("th-%d", tc.pts), ExpectedPoints: tc.pts})
		if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || reasonOf(err) != ReasonQuoteChanged || !strings.Contains(msg, tc.want) {
			t.Fatalf("expectedPoints=%d 应 409 QUOTE_CHANGED「%s」：%v", tc.pts, tc.want, err)
		}
	}
	if j, _ := e.store.GetJobByRequest(e.ctx, uid, "th-0"); j != nil || e.ledger.holds != 0 {
		t.Fatal("被拦下时不建 job、不预留点数")
	}
	jv, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p.PlanNo, PlanHash: p.PlanHash, Stage: "first", IdempotencyKey: "th-ok", ExpectedPoints: 120})
	if err != nil || jv == nil {
		t.Fatalf("带上一致的点数应放行：%v", err)
	}
}

func TestConfirmThresholdAllStageComparesTotal(t *testing.T) {
	e := newEnv(t, true)
	e.withEta(nil, func(c *Config) { c.ConfirmThresholdPoints = 300 })
	p := e.plan(e.seedAnalysis(32, 12, 25))
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p.PlanNo, PlanHash: p.PlanHash, Stage: "all", IdempotencyKey: "all-1", ExpectedPoints: 120})
	if reasonOf(err) != ReasonQuoteChanged {
		t.Fatalf("全部做应与合计 340 比对：%v", err)
	}
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p.PlanNo, PlanHash: p.PlanHash, Stage: "all", IdempotencyKey: "all-2", ExpectedPoints: 340}); err != nil {
		t.Fatalf("一致应放行：%v", err)
	}
}

func TestConfirmThresholdNotReachedKeepsOldRules(t *testing.T) {
	for _, th := range []int64{0, 340, 1000} { // 0 = 不启用；方案总价 340 不「超过」340
		e := newEnv(t, true)
		e.withEta(nil, func(c *Config) { c.ConfirmThresholdPoints = th })
		p := e.plan(e.seedAnalysis(32, 12, 25))
		// 价不一致：仍是 QUOTE_CHANGED
		if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p.PlanNo, PlanHash: p.PlanHash, Stage: "first", IdempotencyKey: "q-1", ExpectedPoints: 130}); reasonOf(err) != "QUOTE_CHANGED" {
			t.Fatalf("阈值 %d：价变应回 QUOTE_CHANGED：%v", th, err)
		}
		// 不带点数（内核直调）：沿用旧口径放行
		if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p.PlanNo, PlanHash: p.PlanHash, Stage: "first", IdempotencyKey: "q-2"}); err != nil {
			t.Fatalf("阈值 %d：未超过阈值不强制 expectedPoints：%v", th, err)
		}
	}
}

// ---- 分析纠偏规则接入 ----

const visionNeedsFix = `{"type":"product","purpose":"sell","audio_kind":"speech",
 "dims":{"product":{"present":true,"desc":"白色运动鞋","risk":"mid"},"line":{"present":true,"desc":""}},
 "sections":[{"t0":0,"t1":2,"desc":"a"},{"t0":2,"t1":2.1,"desc":"编的"},{"t0":2.1,"t1":5,"desc":"c"}],
 "on_screen_text":[],"lines":[]}`

func TestAnalysisRulesAppliedOnRead(t *testing.T) {
	e := newEnv(t, false)
	// 原片 10 秒，画面理解按 5 秒写（时间轴压缩一半）；转写跑了但一句台词也没有
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 10, Width: 720, Height: 1280, HasAudio: true}
	e.gw.analysis["chat.completion"] = json.RawMessage(`{"operation":"chat.completion","message":{"content":"` + jsonEscape(visionNeedsFix) + `"}}`)
	e.gw.analysis["asr.transcribe"] = json.RawMessage(`{"operation":"asr.transcribe","text":"","sentences":[]}`)
	v, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("fix")})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	a, _ = e.store.GetAnalysis(e.ctx, a.ID)

	// 落库的是未纠偏的原始结果 + 画面理解原文
	var raw AnalysisResult
	if err := json.Unmarshal(a.Result, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Corrections) != 0 || raw.RulesVersion != "" || len(raw.Sections) != 3 || raw.AudioKind != "speech" {
		t.Fatalf("落库应是未纠偏的原始结果：%+v", raw)
	}
	var st AnalysisState
	if err := json.Unmarshal(a.State, &st); err != nil || st.VisionContent == "" {
		t.Fatalf("应保存画面理解原文：%v", err)
	}

	// 读取时按当前规则套用
	r, err := e.svc.ResolveAnalysis(a)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range r.Corrections {
		got[c.Rule] = true
	}
	for _, k := range []string{analysisfix.RuleDurationScale, analysisfix.RuleShotNoKeyframe, analysisfix.RuleSpeechNoLines} {
		if !got[k] {
			t.Fatalf("缺纠偏 %s：%+v", k, r.Corrections)
		}
	}
	if r.RulesVersion != analysisfix.Version {
		t.Fatalf("规则版本应记进读取结果：%s", r.RulesVersion)
	}
	// 关键帧在 0、1.11、2.22、3.33、4.44…（9 张 / 10 秒）：×2 缩放后 [4,4.2] 没有关键帧 → 并入前一个，剩 2 个镜头，覆盖 [0,10]
	if len(r.Sections) != 2 || r.Sections[0].T1 != 4.2 || r.Sections[1].T1 != 10 {
		t.Fatalf("镜头：%+v", r.Sections)
	}
	if r.AudioKind != "music" || r.Dims["line"].Present {
		t.Fatalf("有人声但无台词应改正：%s %+v", r.AudioKind, r.Dims["line"])
	}
	// 用户视图：纠偏后的结果，不带纠偏明细
	uv, _ := e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	if uv.Result == nil || len(uv.Result.Corrections) != 0 || uv.Result.RulesVersion == "" || len(uv.Result.Sections) != 2 || uv.Result.AudioKind != "music" {
		t.Fatalf("用户视图应是纠偏后的结果且去掉纠偏明细：%+v", uv.Result)
	}

	// 改规则配置（关掉「镜头没有关键帧」）：已有分析不重跑、不调网关，读取即按新规则
	submits := len(e.gw.submits)
	e.rebuild("test-owner", func(c *Config) {
		c.AnalysisRules = analysisfix.Config{Disabled: []string{analysisfix.RuleShotNoKeyframe}}
	})
	uv2, _ := e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	if uv2.Result == nil || len(uv2.Result.Sections) != 3 || uv2.Result.Sections[1].T0 != 4 || uv2.Result.Sections[1].T1 != 4.2 {
		t.Fatalf("改规则后读取应按新规则（不再并镜头，仍等比放大）：%+v", uv2.Result)
	}
	r2, _ := e.svc.ResolveAnalysis(a)
	if r2.RulesVersion != e.svc.RulesVersion() || r2.RulesVersion == analysisfix.Version {
		t.Fatalf("读取结果应带新规则版本：%s", r2.RulesVersion)
	}
	if len(e.gw.submits) != submits {
		t.Fatalf("改规则不应重新调用网关：%d → %d", submits, len(e.gw.submits))
	}
}

// 没有画面理解原文（直接写入结果的数据）：原样返回落库结果。
func TestResolveAnalysisWithoutVisionContentReturnsStored(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(10)
	r, err := e.svc.ResolveAnalysis(a)
	if err != nil || len(r.Sections) != 1 || r.RulesVersion != "" || len(r.Corrections) != 0 {
		t.Fatalf("应原样返回：%+v %v", r, err)
	}
	if _, err := ResolveAnalysisWith(&model.Analysis{}, analysisfix.Config{}); err == nil {
		t.Fatal("空结果应报错")
	}
}

func TestAnalysisCacheKeyIgnoresRulesVersion(t *testing.T) {
	e := newEnv(t, false)
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 12.4, Width: 720, Height: 1280, HasAudio: true}
	setupAnalysisGW(e)
	src := "mr://" + testRid("cachekey")
	v1, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: src})
	a1, _ := e.store.GetAnalysisByNo(e.ctx, uid, v1.AnalysisNo)
	e.drainAnalysis(a1.ID)
	a1, _ = e.store.GetAnalysis(e.ctx, a1.ID)
	if sourceKey(src) != a1.SourceKey {
		t.Fatal("缓存键只由源地址决定")
	}
	// 同配置：命中缓存
	if v, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: src}); !v.Cached || v.AnalysisNo != v1.AnalysisNo {
		t.Fatalf("同规则版本应命中缓存：%+v", v)
	}
	// 改了规则配置（关掉一条）：缓存键不变，仍命中已有分析，不重新调用网关
	submits := len(e.gw.submits)
	e.rebuild("test-owner", func(c *Config) {
		c.AnalysisRules = analysisfix.Config{Disabled: []string{analysisfix.RuleLineDuplicate}}
	})
	v2, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: src})
	if !v2.Cached || v2.AnalysisNo != v1.AnalysisNo {
		t.Fatalf("规则版本变了也应复用已有分析：%+v", v2)
	}
	a2, _ := e.store.GetAnalysisByNo(e.ctx, uid, v2.AnalysisNo)
	if a2.SourceKey != a1.SourceKey || a2.Attempt != a1.Attempt || len(e.gw.submits) != submits {
		t.Fatalf("已有分析不应失效 / 重跑：key %s→%s attempt %d→%d 调用 %d→%d", a1.SourceKey, a2.SourceKey, a1.Attempt, a2.Attempt, submits, len(e.gw.submits))
	}
}
