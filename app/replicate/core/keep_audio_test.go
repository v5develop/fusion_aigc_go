package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/mediasvc"
)

// 方案 audio=keep 的场景：annotations.audio 决定 JobInput.Audio。
func (e *env) planAudio(a *model.Analysis, audio string, texts []TextOverlay) *PlanView {
	e.t.Helper()
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}, Audio: audio, Texts: texts}})
	if err != nil {
		e.t.Fatalf("CreatePlan: %v", err)
	}
	return pv
}

// 多段 keep：拼接不去声（失败时回落带生成声音的成片），拼好后用原片 [0,32] 整段音轨贴回（首尾相接的区间合并成一段）。
func TestKeepAudioMultiSegmentMuxAfterConcat(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	if jv.Status != "succeeded" || jv.Result == nil || jv.Result.Status != ResultReady {
		t.Fatalf("成片应可用：%s %+v", jv.Status, jv.Result)
	}
	if len(fc.drops) != 1 || fc.drops[0] {
		t.Fatalf("keep 拼接不应去声（贴回失败要回落到生成声音）：%v", fc.drops)
	}
	if len(fc.muxes) != 1 {
		t.Fatalf("应贴回一次：%+v", fc.muxes)
	}
	m := fc.muxes[0]
	if m.source != "src-rid" || m.video != "final-1" || len(m.ranges) != 1 || m.ranges[0].Start != 0 || m.ranges[0].End != 32 {
		t.Fatalf("贴回参数：%+v", m)
	}
	if jv.Result.VideoRid != "mux(final-1)" || jv.Result.Audio != AudioResultKept || jv.Result.AudioNote != "" {
		t.Fatalf("成片应是贴回后的视频：%+v", jv.Result)
	}
}

// 贴回在叠字之前：叠字作用在已贴回原声的视频上。
func TestKeepAudioMuxBeforeTextOverlay(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "font-rid")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, []TextOverlay{{Text: "悦足堂"}}), "all", "k1").JobID, 6)
	if len(fc.muxes) != 1 || len(fc.overlays) != 1 || jv.Result.VideoRid != "text(mux(final-1))" {
		t.Fatalf("先贴回再叠字：muxes=%d overlays=%d rid=%s", len(fc.muxes), len(fc.overlays), jv.Result.VideoRid)
	}
}

// 单段且无叠字：原本不调媒体服务，keep 时也要贴回。
func TestKeepAudioSingleSegmentShortcutStillMuxes(t *testing.T) {
	e := newEnv(t, false)
	fc := withComposer(e, "")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(12.3), AudioKeep, nil), "", "k1").JobID, 3)
	if len(fc.concats) != 0 || len(fc.muxes) != 1 {
		t.Fatalf("单段不拼接但要贴回：concats=%d muxes=%d", len(fc.concats), len(fc.muxes))
	}
	if r := fc.muxes[0].ranges; len(r) != 1 || r[0].Start != 0 || r[0].End != 12.3 {
		t.Fatalf("区间：%+v", r)
	}
	if jv.Result.VideoRid != "mux("+jv.Segments[0].VideoRid+")" || jv.Result.Audio != AudioResultKept {
		t.Fatalf("成片：%+v", jv.Result)
	}
}

// regenerate / mute 不贴回；mute 仍然去声。
func TestKeepAudioNotAppliedForOtherModes(t *testing.T) {
	for _, mode := range []string{AudioRegenerate, AudioMute} {
		e := newEnv(t, true)
		fc := withComposer(e, "")
		jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), mode, nil), "all", "k1").JobID, 6)
		if len(fc.muxes) != 0 || jv.Result.Audio != "" || jv.Result.AudioNote != "" {
			t.Fatalf("%s 不应贴回：%d %+v", mode, len(fc.muxes), jv.Result)
		}
		if want := mode == AudioMute; len(fc.drops) != 1 || fc.drops[0] != want {
			t.Fatalf("%s 的 DropAudio 应为 %v：%v", mode, want, fc.drops)
		}
	}
}

// 贴回失败：回落到未贴回的拼接成片（交付不丢；keep 的段视频本身没有音轨，所以这条成片是静音的），结果视图如实说明并提示可免费重做声音。
func TestKeepAudioMuxFailureFallsBack(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	fc.muxErr = errors.New("media 503")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	if jv.Status != "succeeded" || jv.Result == nil || jv.Result.Status != ResultReady || jv.Result.VideoRid != "final-1" {
		t.Fatalf("贴回失败应回落到拼接成片：%s %+v", jv.Status, jv.Result)
	}
	if jv.Result.Audio != AudioResultFailed || jv.Result.AudioNote != audioFailedNote || !jv.Result.CanRetryAudio {
		t.Fatalf("应如实告诉用户成片没有声音、可免费重做：%+v", jv.Result)
	}
	if len(fc.drops) != 1 || fc.drops[0] || len(fc.muxes) != 0 {
		t.Fatalf("回落的拼接成片不去声、也没贴回：drops=%v muxes=%d", fc.drops, len(fc.muxes))
	}
	e.assertMoney(jv.JobID)
}

// 原片本身没有音轨：不算故障，如实说明。
func TestKeepAudioSourceWithoutAudio(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	fc.muxErr = &mediasvc.Error{Status: 422, Code: "source_rejected", Check: "audio.no_audio_stream"}
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	if jv.Result.VideoRid != "final-1" || jv.Result.Audio != AudioResultNone || !strings.Contains(jv.Result.AudioNote, "本身没有声音") {
		t.Fatalf("%+v", jv.Result)
	}
}

// 合成器没实现贴回：回落，不拦交付。
func TestKeepAudioComposerWithoutMuxer(t *testing.T) {
	e := newEnv(t, false)
	svc := e.svc
	WithComposer(noMuxComposer{&fakeComposer{}})(svc)
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(12.3), AudioKeep, nil), "", "k1").JobID, 3)
	if jv.Result == nil || jv.Result.Status != ResultReady || jv.Result.Audio != AudioResultFailed {
		t.Fatalf("%+v", jv.Result)
	}
}

// noMuxComposer 只暴露 Composer 的三个方法，不带 MuxAudio。
type noMuxComposer struct{ c *fakeComposer }

func (n noMuxComposer) Cut(ctx context.Context, s string, a, b float64, i string) (string, error) {
	return n.c.Cut(ctx, s, a, b, i)
}
func (n noMuxComposer) ConcatItems(ctx context.Context, it []mediasvc.ConcatItem, p mediasvc.ConcatParams, i string) (mediasvc.ConcatResult, error) {
	return n.c.ConcatItems(ctx, it, p, i)
}
func (n noMuxComposer) OverlayText(ctx context.Context, r string, sl []mediasvc.TextSlot, i string) (mediasvc.Resource, error) {
	return n.c.OverlayText(ctx, r, sl, i)
}

// keepRanges：不继续 / 取消的段不进区间；占位段照常对应原声；相邻连续区间合并，不连续的保持分开。
func TestKeepRanges(t *testing.T) {
	seg := func(i int, t0, t1 float64) JobSegment {
		var s JobSegment
		s.Index, s.T0, s.T1 = i, t0, t1
		return s
	}
	outs := []segOutcome{
		{seg: seg(1, 0, 10), rid: "a"},
		{seg: seg(2, 10, 20), failed: true}, // 占位
		{seg: seg(3, 20, 30), rid: "c"},
		{seg: seg(4, 30, 40), omitted: true},
	}
	got := keepRanges(outs)
	if len(got) != 1 || got[0].Start != 0 || got[0].End != 30 {
		t.Fatalf("应合并成 [0,30] 且略过未做的段：%+v", got)
	}
	got = keepRanges([]segOutcome{{seg: seg(1, 0, 10), rid: "a"}, {seg: seg(2, 10, 20), omitted: true}, {seg: seg(3, 20, 30), rid: "c"}})
	if len(got) != 2 || got[1].Start != 20 || got[1].End != 30 {
		t.Fatalf("中间略过的段让区间断开：%+v", got)
	}
}
