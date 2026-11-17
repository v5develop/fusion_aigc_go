package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
)

// 画面连贯度（CONTRACT §Y）的测试。

// smoothPlan 三段（32 秒、切点 12 / 25）的 smooth 方案。
func (e *env) smoothPlan(refs int) *PlanView {
	e.t.Helper()
	rs := make([]string, refs)
	for i := range rs {
		rs[i] = testRef
	}
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: e.seedAnalysis(32, 12, 25).AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: rs,
		Continuity: ContinuitySmooth, Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil {
		e.t.Fatalf("CreatePlan: %v", err)
	}
	return pv
}

func (e *env) reqInput(suffix string) map[string]any {
	e.t.Helper()
	for k, r := range e.gw.reqs {
		if strings.HasSuffix(k, suffix) {
			return r.Input.(map[string]any)
		}
	}
	e.t.Fatalf("没有提交过 %s：%v", suffix, e.gw.submits)
	return nil
}

func imgRids(in map[string]any) []string {
	var out []string
	if imgs, ok := in["reference_images"].([]aigateway.MediaRef); ok {
		for _, m := range imgs {
			out = append(out, m.URI)
		}
	}
	return out
}

// lastFrameOf 某段步骤（按幂等键后缀）的末帧 rid（fakeGW 按步骤键派生）。
func lastFrameOf(stepKey string) string { return aigateway.MRScheme + workRid("lf|"+stepKey) }

func (e *env) stepOf(jobID int64, key string) *model.Step {
	for _, st := range e.steps(jobID) {
		if st.StepKey == key {
			return st
		}
	}
	e.t.Fatalf("没有步骤 %s", key)
	return nil
}

// fast 与缺省 / 单段 smooth：JSON 与 plan_hash 都与旧版逐字节一致。
func TestContinuityFastPlanHashUnchanged(t *testing.T) {
	// 旧版 Annotations 的序列化字节（没有 continuity 键）
	old := `{"dims":{"product":{"mode":"replace","refs":[1]}},"audio":"regenerate"}`
	b, _ := json.Marshal(Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}, Audio: AudioRegenerate})
	if string(b) != old {
		t.Fatalf("fast / 缺省不得出现 continuity 键：\n got=%s\nwant=%s", b, old)
	}
	b, _ = json.Marshal(Annotations{Audio: AudioRegenerate, Continuity: ContinuitySmooth})
	if !strings.Contains(string(b), `"continuity":"smooth"`) {
		t.Fatalf("smooth 应落库：%s", b)
	}

	e := newEnv(t, true)
	a := e.seedAnalysis(32, 12, 25)
	mk := func(c string) *PlanView {
		pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef}, Continuity: c,
			Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
		if err != nil {
			t.Fatalf("CreatePlan(%q): %v", c, err)
		}
		return pv
	}
	def, fast, smooth := mk(""), mk("fast"), mk("smooth")
	if def.PlanHash != fast.PlanHash || def.PlanNo != fast.PlanNo {
		t.Fatal("fast 与缺省必须是同一张方案（同 plan_hash）")
	}
	if def.Continuity != ContinuityFast || smooth.Continuity != ContinuitySmooth || smooth.PlanHash == def.PlanHash {
		t.Fatalf("smooth 应是另一张方案：%s %s", smooth.Continuity, smooth.PlanHash)
	}
	// 单段方案忽略 smooth：指纹与不选一致
	e2 := newEnv(t, true)
	a2 := e2.seedAnalysis(10)
	one := func(c string) *PlanView {
		pv, err := e2.svc.CreatePlan(e2.ctx, PlanReq{UserID: uid, AnalysisNo: a2.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef}, Continuity: c,
			Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return pv
	}
	if x, y := one(""), one("smooth"); x.PlanHash != y.PlanHash || y.Continuity != ContinuityFast || y.ETA.Smooth != nil {
		t.Fatalf("单段方案应忽略连贯度：%+v", y)
	}
	// 非法值
	_, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef}, Continuity: "slow",
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if ge, ok := err.(*GapsError); !ok || ge.Gaps[0].Code != "CONTINUITY_INVALID" {
		t.Fatalf("非法连贯度应给缺项：%v", err)
	}
}

// 改标注：不给 continuity 沿用原方案；给了覆盖；批量 / 再做一条走同一路径继承。
func TestContinuityPatchInherits(t *testing.T) {
	e := newEnv(t, true)
	e.withLib()
	pv := e.smoothPlan(1)
	v, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: Annotations{Dims: map[string]DimEdit{"scene": {Mode: ModeDescribe, Note: "海边"}}}})
	if err != nil || v.Continuity != ContinuitySmooth {
		t.Fatalf("改标注应沿用 smooth：%v %+v", err, v)
	}
	fast := ContinuityFast
	v2, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: v.PlanNo, Continuity: &fast, Annotations: Annotations{Dims: map[string]DimEdit{"scene": {Mode: ModeDescribe, Note: "海边"}}}})
	if err != nil || v2.Continuity != ContinuityFast {
		t.Fatalf("显式 fast 应覆盖：%v %+v", err, v2)
	}
}

// smooth：第 k 段等第 k−1 段到终态才提交；成功就带上一段末帧（最后一张参考图，衔接句里的图号对得上）。
func TestSmoothSchedulesSequentiallyWithBridge(t *testing.T) {
	e := newEnv(t, true)
	pv := e.smoothPlan(1)
	jv := e.confirm(pv, "all", "k1")
	if jv.Continuity != ContinuitySmooth || jv.ETA == nil {
		t.Fatalf("任务视图应带连贯度与 ETA：%+v", jv)
	}
	_ = e.svc.Advance(e.ctx, jv.JobID)
	if e.gw.submitted("seg/01/generate") != 1 || e.gw.submitted("seg/02/generate") != 0 || e.gw.submitted("seg/03/generate") != 0 {
		t.Fatalf("第一轮只应提交第 1 段：%v", e.gw.submits)
	}
	for _, st := range e.steps(jv.JobID) {
		if st.Kind == model.StepKindGateway && st.SegIndex > 1 && st.Status != model.StepPending {
			t.Fatalf("后面的段在上一段做完前必须保持排队：%+v", st)
		}
	}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("应全部成功：%+v", jv)
	}
	// 提交顺序：1 → 2 → 3
	var order []string
	for _, k := range e.gw.submits {
		if strings.Contains(k, "/generate") {
			order = append(order, k[strings.LastIndex(k, "seg/"):])
		}
	}
	if fmt.Sprint(order) != "[seg/01/generate seg/02/generate seg/03/generate]" {
		t.Fatalf("提交顺序：%v", order)
	}
	// 第 1 段没有衔接帧；第 2 / 3 段分别带上一段末帧，且是最后一张
	if got := imgRids(e.reqInput("seg/01/generate")); len(got) != 1 {
		t.Fatalf("第 1 段只有方案参考图：%v", got)
	}
	for k, prev := range map[int]string{2: "seg/01/generate", 3: "seg/02/generate"} {
		in := e.reqInput(fmt.Sprintf("seg/%02d/generate", k))
		got := imgRids(in)
		if len(got) != 2 || got[1] != lastFrameOf(prev) {
			t.Fatalf("第 %d 段应追加第 %s 段末帧作最后一张：%v", k, prev, got)
		}
		p := fmt.Sprint(in["prompt"])
		if !strings.Contains(p, "@图片2（上一段的最后一帧）作为本段开场") {
			t.Fatalf("衔接句的图号应是 2（方案 1 张图 + 衔接帧）：%q", p)
		}
		if k == 2 && strings.Contains(fmt.Sprint(e.reqInput("seg/01/generate")["prompt"]), "上一段的最后一帧") {
			t.Fatal("第 1 段不应有衔接句")
		}
	}
	for k, want := range map[int]string{1: "", 2: "used", 3: "used"} {
		if jv.Segments[k-1].Bridge != want {
			t.Fatalf("第 %d 段 bridge=%q 期望 %q", k, jv.Segments[k-1].Bridge, want)
		}
	}
	e.assertMoney(jv.JobID)
}

// 不同方言用各自的标签写法。
func TestBridgeLineUsesDialectTag(t *testing.T) {
	for group, want := range map[string]string{
		"vid.seedance-2.5": "@图片4（上一段的最后一帧）", "vid.seedance-2": "@图片4（上一段的最后一帧）",
		"vid.wan-3.0": "图4（上一段的最后一帧）", "vid.minimax-h3": "<Picture 4> (the last frame of the previous segment)", "other": "@图片4（上一段的最后一帧）",
	} {
		if got := bridgeLine(DialectFor(group), 4); !strings.Contains(got, want) {
			t.Errorf("%s：%q 缺 %q", group, got, want)
		}
	}
}

// 上一段失败：本段照常生成、不带衔接帧，不阻塞，并留下原因。
func TestSmoothPrevFailedNoBridge(t *testing.T) {
	e := newEnv(t, true)
	jv := e.confirm(e.smoothPlan(1), "all", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	jv = e.run(jv.JobID, 10)
	if jv.Status != "partial" || jv.SegFailed != 1 || jv.SegDone != 2 {
		t.Fatalf("第 1 段失败其余应照常做完：%+v", jv)
	}
	if got := imgRids(e.reqInput("seg/02/generate")); len(got) != 1 {
		t.Fatalf("上一段失败不带衔接帧：%v", got)
	}
	if strings.Contains(fmt.Sprint(e.reqInput("seg/02/generate")["prompt"]), "上一段的最后一帧") {
		t.Fatal("没有衔接帧就不能有衔接句")
	}
	if jv.Segments[1].Bridge != "skipped_prev_failed" {
		t.Fatalf("应记下原因：%q", jv.Segments[1].Bridge)
	}
	// 第 3 段的上一段（第 2 段）成功：照常衔接
	if got := imgRids(e.reqInput("seg/03/generate")); len(got) != 2 || got[1] != lastFrameOf("seg/02/generate") {
		t.Fatalf("第 3 段应衔接第 2 段：%v", got)
	}
}

// 上一段成功但网关没给末帧：同样照常生成。
func TestSmoothNoLastFrameNoBridge(t *testing.T) {
	in := &JobInput{Continuity: ContinuitySmooth, RefImages: []RefImage{{Index: 1, Rid: "r"}}}
	steps := []*model.Step{{Kind: model.StepKindGateway, SegIndex: 1, Status: model.StepSucceeded, Output: json.RawMessage(`{"videoRid":"v1"}`)}}
	if rid, skip := decideBridge(in, steps, 2); rid != "" || skip != bridgeSkipNoFrame {
		t.Fatalf("无末帧：%q %q", rid, skip)
	}
	steps[0].Output = json.RawMessage(`{"videoRid":"v1","lastFrameRid":"lf1"}`)
	if rid, skip := decideBridge(in, steps, 2); rid != "lf1" || skip != "" {
		t.Fatalf("有末帧：%q %q", rid, skip)
	}
	if rid, skip := decideBridge(&JobInput{}, steps, 2); rid != "" || skip != "" {
		t.Fatal("fast 不衔接也不记原因")
	}
}

// 参考图已满：不带衔接帧，方案卡 risks 提前说明。
func TestSmoothRefsAtLimitNoBridgeAndRisk(t *testing.T) {
	e := newEnv(t, true)
	pv := e.smoothPlan(maxRefImages)
	found := false
	for _, r := range pv.Risks {
		if strings.Contains(r, "参考图已满，连贯模式这次不能用上一段画面衔接") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应提示参考图已满：%v", pv.Risks)
	}
	jv := e.run(e.confirm(pv, "all", "k1").JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	if got := imgRids(e.reqInput("seg/02/generate")); len(got) != maxRefImages {
		t.Fatalf("参考图满了不能再追加：%d", len(got))
	}
	if jv.Segments[1].Bridge != "skipped_refs_full" {
		t.Fatalf("bridge=%q", jv.Segments[1].Bridge)
	}
	// 仍是按顺序做（连贯模式的顺序不因为没带衔接帧而改变）
	if e.gw.submitted("seg/03/generate") != 1 {
		t.Fatal("第 3 段应提交一次")
	}
	// 没满的方案没有这条风险
	e2 := newEnv(t, true)
	for _, r := range e2.smoothPlan(1).Risks {
		if strings.Contains(r, "参考图已满") {
			t.Fatalf("不该有：%s", r)
		}
	}
}

// 精修第 k 段用第 k−1 段「当前有效结果」的末帧（k−1 被精修过就用新版本）；不级联重做后续段。
func TestSmoothRefineUsesCurrentPrevResultNoCascade(t *testing.T) {
	e, _, _ := autoDeliveryEnv(t, nil)
	pv := e.smoothPlan(1)
	jv := e.run(e.confirm(pv, "all", "k1").JobID, 10)
	if jv.Status != "succeeded" {
		t.Fatalf("前置：%+v", jv)
	}
	// 精修第 1 段 → 新版本有新的末帧
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1", Note: "动作大一点", ExpectedPoints: jv.Segments[0].Points}); err != nil {
		t.Fatalf("精修第 1 段：%v", err)
	}
	jv = e.run(jv.JobID, 8)
	seg3Before := e.gw.submitted("seg/03/generate")
	// 再精修第 2 段
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r2", Note: "表情自然一点", ExpectedPoints: jv.Segments[1].Points}); err != nil {
		t.Fatalf("精修第 2 段：%v", err)
	}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	got := imgRids(e.reqInput("seg/02/generate#u1"))
	if len(got) != 2 || got[1] != lastFrameOf("seg/01/generate#u1") {
		t.Fatalf("精修第 2 段应用第 1 段【精修后】版本的末帧：%v", got)
	}
	if got[1] == lastFrameOf("seg/01/generate") {
		t.Fatal("不得还用第 1 段的旧版本末帧")
	}
	if e.gw.submitted("seg/03/generate") != seg3Before || e.gw.submitted("seg/03/generate#u1") != 0 {
		t.Fatal("不级联重做后续段")
	}
}

// 失败段重做：第 k−1 段当前有效结果的末帧；k−1 失败过的段重做时 k−1 还是失败则不带。
func TestSmoothRedoFailedSegmentBridgesToCurrentPrev(t *testing.T) {
	e := newEnv(t, true)
	jv := e.confirm(e.smoothPlan(1), "all", "k1")
	e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/02/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	jv = e.run(jv.JobID, 10)
	if jv.Status != "partial" {
		t.Fatalf("前置：%+v", jv)
	}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", ExpectedPoints: jv.Segments[1].Points}); err != nil {
		t.Fatalf("重做：%v", err)
	}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	if got := imgRids(e.reqInput("seg/02/generate#u1")); len(got) != 2 || got[1] != lastFrameOf("seg/01/generate") {
		t.Fatalf("重做第 2 段应带第 1 段末帧：%v", got)
	}
}

// 先做第 1 段 → 继续：第 2 段起按顺序，第 2 段用第 1 段末帧。
func TestSmoothStagedConfirmPath(t *testing.T) {
	e := newEnv(t, true)
	pv := e.smoothPlan(1)
	jv := e.confirm(pv, "", "k1")
	jv = e.run(jv.JobID, 3)
	if jv.Status != "waiting" || e.gw.submitted("seg/02/generate") != 0 {
		t.Fatalf("先做第 1 段：%+v", jv)
	}
	if jv.ETA == nil || jv.ETA.Smooth == nil {
		t.Fatalf("待继续的任务视图也带 ETA：%+v", jv.ETA)
	}
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1", ExpectedPoints: pv.Cost.Rest}); err != nil {
		t.Fatalf("继续：%v", err)
	}
	_ = e.svc.Advance(e.ctx, jv.JobID)
	if e.gw.submitted("seg/02/generate") != 1 || e.gw.submitted("seg/03/generate") != 0 {
		t.Fatalf("继续后第 2 段先、第 3 段等第 2 段：%v", e.gw.submits)
	}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	if got := imgRids(e.reqInput("seg/02/generate")); len(got) != 2 || got[1] != lastFrameOf("seg/01/generate") {
		t.Fatalf("第 2 段应衔接第 1 段末帧：%v", got)
	}
	e.assertMoney(jv.JobID)
}

// 在途 / 排队的段在等上一段时不超时、预留不丢：网关时限从真正提交那刻起算，排队等待期间步骤保持 PENDING。
func TestSmoothWaitingSegmentDeadlineStartsAtSubmit(t *testing.T) {
	e := newEnv(t, true)
	jv := e.confirm(e.smoothPlan(1), "all", "k1")
	_ = e.svc.Advance(e.ctx, jv.JobID)
	e.clock = e.clock.Add(20 * time.Minute) // 第 1 段还在做，第 2 段等了 20 分钟
	for _, st := range e.steps(jv.JobID) {
		if st.SegIndex == 2 && st.Status != model.StepPending {
			t.Fatalf("等待中的段不应被超时处理：%+v", st)
		}
	}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	dl := e.gw.reqs[e.stepOf(jv.JobID, "seg/02/generate").IdemKey].Constraints.DeadlineAt
	if dl == nil || dl.Before(e.clock.Add(20*time.Minute)) {
		t.Fatalf("第 2 段的网关时限应从提交时算起，而不是确认时：%v 现在=%v", dl, e.clock)
	}
	e.assertMoney(jv.JobID)
}

// ETA：多段方案给 fast / smooth 两个区间，smooth = 段数 × 每段（并行为 轮数 × 每轮）；Total 跟随所选模式。
func TestPlanETAFastAndSmooth(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedAnalysis(79, 13, 26, 39, 52, 65) // 6 段，MaxInFlight=3 → 并行 2 轮
	mk := func(c string) *PlanView {
		pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef}, Continuity: c,
			Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return pv
	}
	fast, smooth := mk(""), mk("smooth")
	cfg := e.svc.cfg
	n := fast.Timeline.Segments
	if len(n) != 6 || fast.ETA.Fast == nil || fast.ETA.Smooth == nil {
		t.Fatalf("多段应有两个区间：%+v", fast.ETA)
	}
	p50, p90 := int(cfg.EtaRoundP50.Seconds()), int(cfg.EtaRoundP90.Seconds())
	if fast.ETA.Fast.LowSec != 2*p50 || fast.ETA.Fast.HighSec != 2*p90 || fast.ETA.Smooth.LowSec != 6*p50 || fast.ETA.Smooth.HighSec != 6*p90 {
		t.Fatalf("先验口径：fast=%+v smooth=%+v（每轮 %d–%d）", fast.ETA.Fast, fast.ETA.Smooth, p50, p90)
	}
	if fast.ETA.Total != *fast.ETA.Fast || smooth.ETA.Total != *smooth.ETA.Smooth || *smooth.ETA.Fast != *fast.ETA.Fast {
		t.Fatalf("Total 应跟随所选模式：fast=%+v smooth=%+v", fast.ETA, smooth.ETA)
	}
	if smooth.ETA.Smooth.HighSec <= smooth.ETA.Fast.HighSec || smooth.ETA.Smooth.Text == "" {
		t.Fatal("连贯模式应更久")
	}
	if smooth.ETA.Rest == nil || smooth.ETA.Rest.LowSec != 5*p50 {
		t.Fatalf("smooth 的其余段按顺序算：%+v", smooth.ETA.Rest)
	}
	// 任务视图按所选模式
	jvF, jvS := e.confirm(fast, "all", "kf"), e.confirm(smooth, "all", "ks")
	if jvF.ETA == nil || jvS.ETA == nil || jvF.ETA.Total.HighSec != fast.ETA.Fast.HighSec || jvS.ETA.Total.HighSec != fast.ETA.Smooth.HighSec {
		t.Fatalf("任务 ETA 按所选模式：fast=%+v smooth=%+v", jvF.ETA, jvS.ETA)
	}
	// 单段方案没有这两个区间
	one := e.plan(e.seedAnalysis(10))
	if one.ETA.Fast != nil || one.ETA.Smooth != nil {
		t.Fatalf("单段不给：%+v", one.ETA)
	}
}

// 漏斗：确认事件带 continuity；报价不变（价格与 fast 方案逐项相同）。
func TestConfirmFunnelCarriesContinuityAndPriceUnchanged(t *testing.T) {
	f := &fakeFunnel{}
	e := newOpsEnv(t, true, nil, func(*env) Option { return WithFunnel(f) })
	a := e.seedAnalysis(32, 12, 25)
	mk := func(c string) *PlanView {
		pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef}, Continuity: c,
			Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return pv
	}
	fast, smooth := mk(""), mk("smooth")
	if fast.StageQuotes != smooth.StageQuotes {
		t.Fatalf("价格不变：fast=%+v smooth=%+v", fast.StageQuotes, smooth.StageQuotes)
	}
	e.confirm(fast, "all", "kf")
	e.confirm(smooth, "all", "ks")
	var got []string
	for _, r := range f.rows {
		if r.Stage == StageConfirmAll {
			got = append(got, r.Continuity)
		}
	}
	if fmt.Sprint(got) != "[fast smooth]" {
		t.Fatalf("confirm 事件的连贯度：%v", got)
	}
	if r := f.find(StagePlanCreate); r == nil || r.Continuity != ContinuityFast {
		t.Fatalf("非确认事件默认 fast：%+v", r)
	}
}
