package core

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/rules/segment"
	"aigc-local-life/pkg/mediasvc"
)

// 接缝挪到画面最静处（CONTRACT §R）：分析阶段算一次曲线存进状态，分段读取时带上；算不出照旧均分。

func TestMotionQuantizeRoundTrip(t *testing.T) {
	in := []float64{0, 0.12, 1.5, 31.9, 200, -3, math.NaN()}
	m := (&MotionState{FPS: 4, Q: quantizeMotion(in)}).toSegment()
	want := []float64{0, 0.125, 1.5, 31.875, 31.875, 0, 0}
	if m == nil || m.FPS != 4 || len(m.Scores) != len(in) {
		t.Fatalf("%+v", m)
	}
	for i, w := range want {
		if math.Abs(m.Scores[i]-w) > 1e-9 {
			t.Fatalf("scores=%v want=%v", m.Scores, want)
		}
	}
	for _, bad := range []*MotionState{nil, {FPS: 0, Q: "AAAA"}, {FPS: 4, Q: ""}, {FPS: 4, Q: "!!"}, {FPS: 4, Q: "AA=="}} {
		if bad.toSegment() != nil {
			t.Fatalf("%+v 应不可用", bad)
		}
	}
}

// profileOf 4 fps 曲线：底噪 5，指定时刻（与其下一个采样点）为 0.1，让那里成为唯一的静处。
func profileFor(d float64, quiet ...float64) mediasvc.MotionProfile {
	sc := make([]float64, int(d*4)+1)
	for i := range sc {
		sc[i] = 5
	}
	for _, q := range quiet {
		i := int(q * 4)
		sc[i], sc[i+1] = 0.1, 0.1
	}
	return mediasvc.MotionProfile{FPS: 4, DurationS: d, Scores: sc}
}

func (e *env) analyzeLong(d float64) (*AnalysisView, int64) {
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: d, Width: 720, Height: 1280, Fps: 30, HasAudio: true}
	setupAnalysisGW(e)
	// 只有一个分镜：没有镜头切换候选，才轮到「画面最静处」
	one := strings.Replace(visionJSON, `"sections":[{"t0":0,"t1":4.2,"desc":"展示鞋","action":"模特展示外套"},{"t0":4,"t1":8,"desc":"近景","action":"近景细节"},{"t0":8,"t1":120,"desc":"走动","action":"全身走动"}]`,
		`"sections":[{"t0":0,"t1":120,"desc":"展示鞋","action":"模特展示外套"}]`, 1)
	e.gw.analysis["chat.completion"] = json.RawMessage(`{"operation":"chat.completion","message":{"content":"` + "```json\\n" + jsonEscape(one) + "\\n```" + `"}}`)
	v, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://own.test/aigc/video/7/20261113/v.mp4?sign=abc"})
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	v, _ = e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	return v, a.ID
}

func TestAnalysisComputesMotionOnceAndPlanCutsAtQuietest(t *testing.T) {
	e := newEnv(t, true)
	e.media.motion = func(string) (mediasvc.MotionProfile, error) { return profileFor(32, 12), nil }
	v, id := e.analyzeLong(32)
	if v.Status != "succeeded" || e.media.motionCalls != 1 {
		t.Fatalf("应成功且只算一次曲线：%s 调用=%d", v.Status, e.media.motionCalls)
	}
	a, _ := e.store.GetAnalysis(e.ctx, id)
	st := mustState(t, a.State)
	if st.Motion == nil || st.Motion.FPS != 4 || len(st.Motion.Q) == 0 {
		t.Fatalf("状态里应存压缩曲线：%+v", st.Motion)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(a.Result, &top); err != nil || len(top) == 0 {
		t.Fatalf("%v", err)
	}
	for k := range top {
		if strings.EqualFold(k, "motion") {
			t.Fatalf("曲线不进结果 JSON：%s", a.Result)
		}
	}
	an, err := e.svc.ResolveAnalysis(a)
	if err != nil || an.Motion == nil {
		t.Fatalf("读取时应带上曲线：%v", err)
	}
	pv := e.plan(a)
	var firstCut float64
	var reason string
	for _, sv := range pv.Timeline.Segments {
		firstCut, reason = sv.T1, sv.CutReason
		break
	}
	if reason != segment.ReasonQuiet || firstCut != 12 {
		t.Fatalf("第 1 个切点应落在最静的 12.0：t=%v reason=%s", firstCut, reason)
	}
	if e.media.motionCalls != 1 {
		t.Fatalf("出方案不再调媒体服务：%d", e.media.motionCalls)
	}
}

// 曲线算不出（媒体服务出错）：分析照常成功，分段退回均分，内部标记不给用户看。
func TestAnalysisMotionFailOpen(t *testing.T) {
	e := newEnv(t, true)
	e.media.motion = func(string) (mediasvc.MotionProfile, error) { return mediasvc.MotionProfile{}, errors.New("media 503") }
	v, id := e.analyzeLong(32)
	if v.Status != "succeeded" {
		t.Fatalf("%+v", v)
	}
	for _, w := range v.Warnings {
		if containsAny(w, "motion") {
			t.Fatalf("内部降级标记不应给用户：%v", v.Warnings)
		}
	}
	a, _ := e.store.GetAnalysis(e.ctx, id)
	if mustState(t, a.State).Motion != nil {
		t.Fatal("失败不应存曲线")
	}
	pv := e.plan(a)
	for _, sv := range pv.Timeline.Segments[:1] {
		if sv.CutReason != segment.ReasonEven {
			t.Fatalf("没有曲线应均分：%+v", sv)
		}
	}
}

// 单段视频（≤ 单段上限）没有接缝：不调媒体服务；运行时关闭同样不算。
func TestAnalysisMotionSkippedWhenSingleSegmentOrOff(t *testing.T) {
	e := newEnv(t, true)
	e.media.motion = func(string) (mediasvc.MotionProfile, error) { return profileFor(12), nil }
	e.analyzeLong(12)
	if e.media.motionCalls != 0 {
		t.Fatalf("单段不需要曲线：%d", e.media.motionCalls)
	}
	e2 := newEnv(t, true)
	e2.svc.cfg.MotionOff = true
	e2.media.motion = func(string) (mediasvc.MotionProfile, error) { return profileFor(32), nil }
	e2.analyzeLong(32)
	if e2.media.motionCalls != 0 {
		t.Fatalf("关闭后不算：%d", e2.media.motionCalls)
	}
}

func mustState(t *testing.T, raw []byte) AnalysisState {
	t.Helper()
	var st AnalysisState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
