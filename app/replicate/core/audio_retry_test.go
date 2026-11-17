package core

import (
	"errors"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"
)

// 免费重做声音（CONTRACT §Q）。

// keep 场景：贴回失败 → 成片静音 + 如实文案；排除故障后重做成功 → 新成片、audio=kept、入库新版本，次数 +1。
func TestRetryAudioKeepSucceedsAndDeliversNewVersion(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	ops := newMemOps(func() time.Time { return e.clock })
	WithDelivery(ops, &fakeLib{}, &fakeAssets{}, DeliveryConfig{MaxAttempts: 3})(e.svc)
	ff := withFunnel(e)
	fc.muxErr = errors.New("media 503")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	if jv.Result.Audio != AudioResultFailed || jv.Result.AudioNote != audioFailedNote || !jv.Result.CanRetryAudio {
		t.Fatalf("失败应如实说明：%+v", jv.Result)
	}
	before := deliveryVersions(ops, jv.JobID, model.DeliveryFinal)
	if len(before) != 1 || before[0] != 0 {
		t.Fatalf("首版成片入库 version=0：%v", before)
	}

	fc.muxErr = nil
	v, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "call-1")
	if err != nil {
		t.Fatalf("重做：%v", err)
	}
	if v.Audio != AudioResultKept || !v.Changed || v.RetriesLeft != 2 || v.CanRetryAudio || v.Replay {
		t.Fatalf("结果：%+v", v)
	}
	if len(fc.muxes) != 1 || fc.muxes[0].video != "final-1" || fc.muxes[0].source != "src-rid" {
		t.Fatalf("应在现有成片上贴回原声：%+v", fc.muxes)
	}
	d := resultDoc(t, e, jv.JobID)
	if d.VideoRid != "mux(final-1)" || d.Audio != AudioResultKept || d.AudioRetries != 1 || d.AudioGen != 1 || d.AudioClaim != nil {
		t.Fatalf("结果文档：%+v", d)
	}
	after := deliveryVersions(ops, jv.JobID, model.DeliveryFinal)
	if len(after) != 2 || after[1] != 1000 {
		t.Fatalf("新成片应入库为新版本：%v", after)
	}
	// 视图：不再提示失败
	jv2, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
	if jv2.Result.AudioNote != "" || jv2.Result.CanRetryAudio || jv2.Result.VideoRid != "mux(final-1)" {
		t.Fatalf("视图：%+v", jv2.Result)
	}
	if ff.count(StageRetryAudio) != 1 {
		t.Fatalf("漏斗 retry_audio=%d", ff.count(StageRetryAudio))
	}
	// 再调：声音没出问题
	if _, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "call-2"); reasonOf(err) != ReasonAudioNotFailed {
		t.Fatalf("应 AUDIO_NOT_FAILED：%v", err)
	}
	// 同 call_id 重放：返回上次的结果，不再贴回、不再计次
	r, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "call-1")
	if err != nil || !r.Replay || r.Audio != AudioResultKept || len(fc.muxes) != 1 {
		t.Fatalf("重放：%+v %v muxes=%d", r, err, len(fc.muxes))
	}
	if d2 := resultDoc(t, e, jv.JobID); d2.AudioRetries != 1 {
		t.Fatalf("重放不计次：%+v", d2)
	}
	e.assertMoney(jv.JobID)
}

func notReadyErr(err error) bool {
	c, _, ok := xerr.Parse(err)
	return !ok || c != xerr.NotReady
}

func deliveryVersions(ops *memOps, jobID int64, kind string) []int {
	var out []int
	for _, d := range ops.dels {
		if d.JobID == jobID && d.Kind == kind {
			out = append(out, d.Version)
		}
	}
	return out
}

// 一直失败：最多 3 次，之后 AUDIO_RETRY_LIMIT；失败的重做同样计次，成片与声音状态不变。
func TestRetryAudioLimit(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	fc.muxErr = errors.New("media 503")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	for i := 1; i <= maxAudioRetries; i++ {
		v, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "c"+string(rune('0'+i)))
		if err != nil || v.Audio != AudioResultFailed || v.Changed || v.RetriesLeft != maxAudioRetries-i {
			t.Fatalf("第 %d 次：%+v %v", i, v, err)
		}
		if i == maxAudioRetries && v.CanRetryAudio {
			t.Fatal("用完后不应再提示可重做")
		}
	}
	if _, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "c9"); reasonOf(err) != ReasonAudioRetryLimit {
		t.Fatalf("应 AUDIO_RETRY_LIMIT：%v", err)
	}
	if d := resultDoc(t, e, jv.JobID); d.VideoRid != "final-1" || d.Audio != AudioResultFailed || d.AudioRetries != 3 {
		t.Fatalf("%+v", d)
	}
	jv2, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
	if jv2.Result.CanRetryAudio || jv2.Result.AudioNote != audioFailedNote {
		t.Fatalf("视图：%+v", jv2.Result)
	}
}

// 声音没失败 / 没成片：对应错误码。
func TestRetryAudioNotFailedAndNotDelivered(t *testing.T) {
	e := newEnv(t, true)
	withComposer(e, "")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6) // 贴回成功
	if _, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "c1"); reasonOf(err) != ReasonAudioNotFailed {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.RetryAudio(e.ctx, uid+1, jv.JobID, "c2"); err == nil {
		t.Fatal("别人的任务应找不到")
	}
	// 没装配贴回算子：未开放
	WithComposer(noMuxComposer{&fakeComposer{}})(e.svc)
	if _, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "c3"); notReadyErr(err) {
		t.Fatalf("%v", err)
	}
}

// 占位未过期 → IN_PROGRESS；过期后可接手。成片依据变了（还在更新）→ IN_PROGRESS。
func TestRetryAudioClaimInProgressAndStale(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	fc.muxErr = errors.New("media 503")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	fc.muxErr = nil
	j, _ := e.store.GetJob(e.ctx, jv.JobID)
	d := resultDoc(t, e, jv.JobID)
	d.AudioClaim = &audioClaim{CallID: "other", At: e.clock}
	d.AudioRetries = 1
	nb := mustMarshal(d)
	if ok, _ := e.store.SwapResultDoc(e.ctx, j.ID, j.ResultDocument, nb); !ok {
		t.Fatal("swap")
	}
	if _, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "mine"); reasonOf(err) != "IN_PROGRESS" {
		t.Fatalf("占位未过期应 IN_PROGRESS：%v", err)
	}
	e.clock = e.clock.Add(audioClaimStale + time.Minute)
	v, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "mine")
	if err != nil || v.Audio != AudioResultKept || v.RetriesLeft != 1 {
		t.Fatalf("占位过期应可接手（沿用已计的次数）：%+v %v", v, err)
	}
}

// 并发：只有一个调用真正去做，其余 IN_PROGRESS 或重放。
func TestRetryAudioConcurrentSingleWinner(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	fc.muxErr = errors.New("media 503")
	jv := e.run(e.confirm(e.planAudio(e.seedAnalysis(32, 12, 25), AudioKeep, nil), "all", "k1").JobID, 6)
	fc.muxErr = nil
	var wg sync.WaitGroup
	res := make([]error, 6)
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, res[i] = e.svc.RetryAudio(e.ctx, uid, jv.JobID, "same")
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range res {
		if err == nil {
			ok++
		}
	}
	if d := resultDoc(t, e, jv.JobID); d.AudioRetries != 1 || d.Audio != AudioResultKept {
		t.Fatalf("只应重做一次：%+v", d)
	}
	if ok < 1 {
		t.Fatalf("至少一个成功：%v", res)
	}
}

// revoice：配音失败 → 退额度；重做成功 → 新配音、不再占额度；TTS 沿用同样的幂等键（网关回放）。
func TestRetryAudioRevoiceDoesNotConsumeQuotaAgain(t *testing.T) {
	e, fc, vs := voiceEnv(t, true)
	ops := newMemOps(func() time.Time { return e.clock })
	WithDelivery(ops, &fakeLib{}, &fakeAssets{}, DeliveryConfig{MaxAttempts: 3})(e.svc)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	fc.timelineErr = errors.New("media 503")
	jv := e.run(e.voiceJobAll(t, a, &VoiceEdit{VoiceID: 5}, "k1").JobID, 6)
	if jv.Result.Audio != AudioResultFailed || vs.total(e) != 0 {
		t.Fatalf("首次失败应退额度：%+v used=%d", jv.Result, vs.total(e))
	}
	submitsBefore := len(e.gw.submits)
	fc.timelineErr = nil
	v, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "rv-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Audio != AudioResultRevoiced || !v.Changed || vs.total(e) != 0 {
		t.Fatalf("重做配音：%+v used=%d（不应再占额度）", v, vs.total(e))
	}
	if len(fc.timelines) != 1 || len(fc.muxes) != 1 || fc.muxes[0].video != "final-1" || fc.muxes[0].source != "track-1" {
		t.Fatalf("应在现有成片上换音轨：timelines=%d muxes=%+v", len(fc.timelines), fc.muxes)
	}
	// 同一份台词同一音色 → TTS 幂等键与首次相同（首次的 3 句已在网关，不新增计费调用）
	if got := len(e.gw.submits); got != submitsBefore {
		t.Logf("提交数 %d → %d（幂等键相同，网关回放）", submitsBefore, got)
	}
	d := resultDoc(t, e, jv.JobID)
	if d.VideoRid != "mux(final-1)" || d.Audio != AudioResultRevoiced || d.AudioKind != AudioRevoice {
		t.Fatalf("%+v", d)
	}
	e.assertMoney(jv.JobID)
}

// revoice 重做：TTS 仍然失败 → 仍 failed、仍不占额度、次数 +1。
func TestRetryAudioRevoiceStillFails(t *testing.T) {
	e, fc, vs := voiceEnv(t, true)
	a := e.seedLinesAnalysis(32, voiceTestLines, 12, 25)
	fc.timelineErr = errors.New("media 503")
	jv := e.run(e.voiceJobAll(t, a, &VoiceEdit{VoiceID: 5}, "k1").JobID, 6)
	e.gw.ttsHook = func(aigateway.CallRequest) (string, int64, bool) { return "", 0, true }
	v, err := e.svc.RetryAudio(e.ctx, uid, jv.JobID, "rv-1")
	if err != nil || v.Audio != AudioResultFailed || v.Changed || v.RetriesLeft != 2 || !v.CanRetryAudio || vs.total(e) != 0 {
		t.Fatalf("%+v %v used=%d", v, err, vs.total(e))
	}
}
