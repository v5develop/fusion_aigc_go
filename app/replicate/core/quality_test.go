package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/segment"
)

// 出片质量与可控性：方言选择、写手（成功 / 回落）、模型校验与报价、取消剩余段。

// reconfig 用改过的运行参数重建内核（替身不变）。
func (e *env) reconfig(mut func(*Config)) {
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	mut(&cfg)
	opts := []Option{WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }), WithOwner("test-owner"),
		WithURLOwner(testOwner), WithSigner(e.signer)}
	if e.cutter != nil {
		opts = append(opts, WithCutter(e.cutter))
	}
	e.svc = New(cfg, e.store, e.ledger, opts...)
}

func (e *env) planModel(a *model.Analysis, group, res string) (*PlanView, error) {
	return e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: group, Resolution: res, RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
}

func gapCodes(err error) []string {
	var ge *GapsError
	if !errors.As(err, &ge) {
		return nil
	}
	var out []string
	for _, g := range ge.Gaps {
		out = append(out, g.Code)
	}
	return out
}

// ---- 方言 ----

func TestDialectSelection(t *testing.T) {
	cases := map[string]string{
		"vid.seedance-2.5": "seedance-2.5", "vid.seedance-2.5-prime": "seedance-2.5",
		"vid.seedance-2": "seedance-2", "vid.seedance-2-fast": "seedance-2", "vid.seedance-2-mini": "seedance-2",
		"vid.minimax-h3": "minimax-h3", "vid.wan-3.0": "wan-3.0", "vid.wan-3.0-prime": "wan-3.0",
		"vid.happyhorse-1.1": "base", "": "base",
	}
	for g, want := range cases {
		if got := DialectFor(g).Code; got != want {
			t.Fatalf("DialectFor(%q)=%s，应为 %s", g, got, want)
		}
	}
}

func TestDialectRenderTags(t *testing.T) {
	an := &AnalysisResult{DurationS: 10, DimOrder: DimKeys, Dims: map[string]Dim{"product": {Present: true, Desc: "白色运动鞋"},
		"camera": {Present: true, Desc: "手持跟拍"}, "scene": {Present: true, Desc: "门店"}},
		Sections: []Section{{Index: 1, T0: 0, T1: 5, Desc: "开箱"}, {Index: 2, T0: 5, T1: 10, Desc: "上脚", Camera: "推近"}}}
	ann := Annotations{Audio: AudioRegenerate, Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}
	seg := segment.Segment{Index: 2, GenT0: 0, GenT1: 10, GenDuration: 10}
	want := map[string][]string{
		"vid.seedance-2.5":   {"@视频1", "@图片1", "第 2 段", "不要生成任何文字"},
		"vid.seedance-2":     {"@视频1", "@图片1", "<主体1>"},
		"vid.minimax-h3":     {"<Video 1>", "<Picture 1>", "<Subject 1>", "subject_definitions:", "watermark"},
		"vid.wan-3.0":        {"视频1", "图1", "分镜1", "负向清单"},
		"vid.happyhorse-1.1": {"@视频1", "@图片1"},
	}
	for g, subs := range want {
		p := RenderDialect(DialectFor(g), an, ann, 1, seg, 3)
		for _, sub := range subs {
			if !strings.Contains(p, sub) {
				t.Fatalf("%s 提示词缺「%s」：\n%s", g, sub, p)
			}
		}
		if issues := CheckWritten(DialectFor(g), p, p, 1); len(issues) > 0 {
			t.Fatalf("%s 模板稿自身不过校验：%v", g, issues)
		}
	}
	// 多出一张没挂的图 → 校验报出来
	if issues := CheckWritten(DialectFor("vid.wan-3.0"), "总体描述：参考 视频1 重新生成，图2 是商品，画面中不要生成任何文字。", "图1 视频1", 1); len(issues) < 2 {
		t.Fatalf("应报「引用了没挂上的图2」与「漏掉了图1」：%v", issues)
	}
}

func TestPlanUsesModelDialect(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(10)
	pv, err := e.planModel(a, "vid.minimax-h3", "")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Dialect != "minimax-h3" || !strings.Contains(pv.Prompts[0], "<Picture 1>") {
		t.Fatalf("方言=%s 提示词=%s", pv.Dialect, pv.Prompts[0])
	}
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if p.Resolution != "768p" {
		t.Fatalf("H3 默认清晰度应为 768p，实际 %s", p.Resolution)
	}
}

// ---- 模型校验与报价 ----

func TestModelValidationAndQuoteByModel(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(10)
	if _, err := e.planModel(a, "vid.unknown", ""); !contains(gapCodes(err), "MODEL_UNKNOWN") {
		t.Fatalf("未知模型应 MODEL_UNKNOWN：%v", err)
	}
	if _, err := e.planModel(a, "vid.minimax-h3", "720p"); !contains(gapCodes(err), "RESOLUTION_UNSUPPORTED") {
		t.Fatalf("H3 不支持 720p：%v", err)
	}
	if _, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.wan-3.0", AspectRatio: "21:9", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}}); !contains(gapCodes(err), "ASPECT_UNSUPPORTED") {
		t.Fatalf("万相不支持 21:9：%v", err)
	}
	// 运营只开放两家：其余按未知处理
	e.reconfig(func(c *Config) { c.Models = []string{"vid.seedance-2", "vid.wan-3.0"}; c.DefaultModel = "vid.wan-3.0" })
	if _, err := e.planModel(a, "vid.seedance-2.5", ""); !contains(gapCodes(err), "MODEL_UNKNOWN") {
		t.Fatalf("未开放的模型应 MODEL_UNKNOWN：%v", err)
	}
	if e.svc.Limits().DefaultModel != "vid.wan-3.0" || len(e.svc.Limits().Models) != 2 {
		t.Fatalf("limits：%+v", e.svc.Limits())
	}
	e.reconfig(func(*Config) {})

	// 报价随模型变化
	e.ledger.groupUnit = map[string]int64{"vid.seedance-2.5": 10, "vid.seedance-2": 6, "vid.wan-3.0": 8}
	p1, err := e.planModel(a, "vid.seedance-2.5", "")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.planModel(a, "vid.seedance-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if p1.StageQuotes.Total != 100 || p2.StageQuotes.Total != 60 || p1.PlanHash == p2.PlanHash {
		t.Fatalf("报价应随模型变化：%d / %d", p1.StageQuotes.Total, p2.StageQuotes.Total)
	}
	// 换模型改标注出新方案，报价跟着变（改标注属方案库能力，需装配 LibraryStore，否则 7003）
	e.withLib()
	g := "vid.wan-3.0"
	p3, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: p1.PlanNo, Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}, ModelGroup: &g})
	if err != nil {
		t.Fatal(err)
	}
	if p3.StageQuotes.Total != 80 || p3.Dialect != "wan-3.0" {
		t.Fatalf("换模型后报价 %d 方言 %s", p3.StageQuotes.Total, p3.Dialect)
	}
	// 价变时确认（页面点数是旧模型的）→ 409
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: p3.PlanNo, PlanHash: p3.PlanHash, IdempotencyKey: "k-pc", ExpectedPoints: 100}); err == nil {
		t.Fatal("页面点数与新模型报价不一致应拒收")
	}

	// 模型列表：单价 / 是否支持 / 未配价不可选
	e.ledger.quoteErr = map[string]error{"vid.minimax-h3": errors.New("未配价")}
	list, err := e.svc.ListModels(e.ctx, ModelsReq{UserID: uid, AnalysisNo: a.AnalysisNo, AspectRatio: "21:9"})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ModelView{}
	for _, m := range list.Items {
		by[m.ModelGroup] = m
	}
	if list.DefaultModel != "vid.seedance-2.5" || !by["vid.seedance-2.5"].Recommended {
		t.Fatalf("默认推荐：%+v", list)
	}
	if by["vid.seedance-2"].PointsPerSecond != 6 || by["vid.seedance-2"].EstimatedPoints != 60 {
		t.Fatalf("单价 / 预计：%+v", by["vid.seedance-2"])
	}
	if by["vid.wan-3.0"].SupportsAspect || by["vid.wan-3.0"].Available {
		t.Fatalf("万相不支持 21:9：%+v", by["vid.wan-3.0"])
	}
	if by["vid.minimax-h3"].Available {
		t.Fatalf("未配价应不可选：%+v", by["vid.minimax-h3"])
	}
	if !by["vid.seedance-2.5"].SupportsDuration {
		t.Fatal("10 秒单段应支持")
	}
}

// ---- 写手 ----

func writerEnv(t *testing.T) *env {
	e := newEnv(t, false)
	e.reconfig(func(c *Config) { c.WriterModelGroup = "llm.writer" })
	return e
}

func genPrompt(e *env, jobID int64) string {
	for k, r := range e.gw.reqs {
		if strings.HasSuffix(k, "seg/01/generate") {
			in, _ := r.Input.(map[string]any)
			p, _ := in["prompt"].(string)
			return p
		}
	}
	e.t.Fatalf("没有出片提交")
	return ""
}

func writerOut(e *env, jobID int64) writerOutput {
	var o writerOutput
	for _, st := range e.steps(jobID) {
		if st.Kind == model.StepKindWriter {
			_ = json.Unmarshal(st.Output, &o)
		}
	}
	return o
}

func TestWriterSuccessUsedForGenerate(t *testing.T) {
	e := writerEnv(t)
	written := "以 @视频1 的动作、走位与运镜为参考，把商品换成 @图片1 中的运动鞋，镜头缓慢推近；画面中不要生成任何文字、字幕、水印、Logo。"
	e.gw.analysis["chat.completion"] = json.RawMessage(`{"operation":"chat.completion","message":{"content":"` + written + `"}}`)
	a := e.seedAnalysis(10)
	pv := e.plan(a)
	jv := e.confirm(pv, "", "k-w1")
	var in JobInput
	j, _ := e.store.GetJob(e.ctx, jv.JobID)
	_ = json.Unmarshal(j.Input, &in)
	if in.WriterModelGroup != "llm.writer" || in.Segments[0].WriterBrief == "" || !strings.Contains(in.Segments[0].WriterBrief, "【模板稿】") {
		t.Fatalf("写手输入没冻结：%+v", in)
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "succeeded" {
		t.Fatalf("job=%s", jv.Status)
	}
	if got := genPrompt(e, jv.JobID); got != written {
		t.Fatalf("出片应用写手稿：%s", got)
	}
	if e.gw.submitted(":seg/01/write") != 1 {
		t.Fatalf("写手幂等键应为 {job}:seg/01/write 且只提交一次：%v", e.gw.submits)
	}
	if o := writerOut(e, jv.JobID); o.Source != "writer" {
		t.Fatalf("写手 output：%+v", o)
	}
	e.assertMoney(jv.JobID)
	if e.ledger.sum("CONFIRMED") != 100 {
		t.Fatalf("写手不额外收费：扣除 %d", e.ledger.sum("CONFIRMED"))
	}
}

func TestWriterCallFailureFallsBackToTemplate(t *testing.T) {
	e := writerEnv(t) // 替身对 chat.completion 不返回正文 → 回落
	a := e.seedAnalysis(10)
	pv := e.plan(a)
	jv := e.run(e.confirm(pv, "", "k-w2").JobID, 6)
	if jv.Status != "succeeded" {
		t.Fatalf("回落后照常出片：%s", jv.Status)
	}
	if got := genPrompt(e, jv.JobID); got != pv.Prompts[0] {
		t.Fatalf("应回落模板稿：\n%s\n----\n%s", got, pv.Prompts[0])
	}
	if o := writerOut(e, jv.JobID); o.Source != "template" || o.Reason != "call_failed" {
		t.Fatalf("回落原因：%+v", o)
	}
}

func TestWriterCheckFailureRetriesThenFallsBack(t *testing.T) {
	e := writerEnv(t)
	e.gw.analysis["chat.completion"] = json.RawMessage(`{"operation":"chat.completion","message":{"content":"一条没有引用参考视频也没有引用图片的提示词，内容足够长足够长。"}}`)
	a := e.seedAnalysis(10)
	pv := e.plan(a)
	jv := e.run(e.confirm(pv, "", "k-w3").JobID, 8)
	if e.gw.submitted(":seg/01/write") != 1 || e.gw.submitted(":seg/01/write#a2") != 1 {
		t.Fatalf("应重写一次（#a2）：%v", e.gw.submits)
	}
	o := writerOut(e, jv.JobID)
	if o.Source != "template" || o.Reason != "check_failed" || len(o.Issues) == 0 {
		t.Fatalf("校验不过应回落：%+v", o)
	}
	if genPrompt(e, jv.JobID) != pv.Prompts[0] || jv.Status != "succeeded" {
		t.Fatalf("出片应用模板稿：%s", jv.Status)
	}
}

// ---- 取消剩余段 ----

func TestCancelRestRefundsUnstartedAndKeepsDone(t *testing.T) {
	e := newEnv(t, true)
	e.reconfig(func(c *Config) { c.MaxInFlight = 1 })
	a := e.seedAnalysis(40, 13, 26)
	pv := e.plan(a)
	jv := e.confirm(pv, "all", "k-c1")
	if jv.SegTotal != 3 {
		t.Fatalf("应 3 段：%d", jv.SegTotal)
	}
	if err := e.svc.Advance(e.ctx, jv.JobID); err != nil { // 第 1 段开始生成，2、3 段排队
		t.Fatal(err)
	}
	pre, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
	if pre.CancelRest == nil || !pre.CancelRest.Available || pre.CancelRest.Segments != 2 || pre.CancelRest.Running != 1 {
		t.Fatalf("预览：%+v", pre.CancelRest)
	}
	refundPreview := pre.CancelRest.Points
	v, err := e.svc.CancelRest(e.ctx, uid, jv.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Money.Refunded != refundPreview || refundPreview == 0 {
		t.Fatalf("退回 %d，预览 %d", v.Money.Refunded, refundPreview)
	}
	// 幂等
	v2, err := e.svc.CancelRest(e.ctx, uid, jv.JobID)
	if err != nil || v2.Money.Refunded != refundPreview {
		t.Fatalf("重复点击应幂等：%v %+v", err, v2.Money)
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "partial" || jv.SegDone != 1 {
		t.Fatalf("在途段做完、job 部分成功：%s done=%d", jv.Status, jv.SegDone)
	}
	for _, sg := range jv.Segments[1:] {
		if sg.State != "canceled" || sg.StateLabel != "未做" {
			t.Fatalf("段 %d：%+v", sg.Index, sg)
		}
	}
	e.assertMoney(jv.JobID)
	// 成片不为取消的段占位（只入第 1 段）
	j, _ := e.store.GetJob(e.ctx, jv.JobID)
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	steps := e.steps(jv.JobID)
	for _, o := range e.svc.outcomes(j, &in, steps)[1:] {
		if !o.omitted || o.failed {
			t.Fatalf("取消的段应省略不占位：%+v", o)
		}
	}
	if e.gw.submitted("seg/02/generate") != 0 || e.gw.submitted("seg/03/generate") != 0 {
		t.Fatal("取消的段不应出片")
	}
}

func TestCancelRestAllUnstartedCancelsJob(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedAnalysis(40, 13, 26)
	pv := e.plan(a)
	jv := e.confirm(pv, "all", "k-c2") // 还没推进：三段都没开始
	v, err := e.svc.CancelRest(e.ctx, uid, jv.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "canceled" || v.Money.Refunded != pv.StageQuotes.Total || v.Money.Confirmed != 0 {
		t.Fatalf("全部取消：%s %+v", v.Status, v.Money)
	}
	e.assertMoney(jv.JobID)
	if _, err := e.svc.CancelRest(e.ctx, uid, jv.JobID); err != nil {
		t.Fatalf("终态幂等：%v", err)
	}
}

func TestCancelRestWaitingEqualsStop(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedAnalysis(40, 13, 26)
	pv := e.plan(a)
	jv := e.run(e.confirm(pv, "first", "k-c3").JobID, 4)
	if jv.Status != "waiting" {
		t.Fatalf("应待继续：%s", jv.Status)
	}
	v, err := e.svc.CancelRest(e.ctx, uid, jv.JobID)
	if err != nil || v.Status != "succeeded" {
		t.Fatalf("待继续时等同不继续：%v %s", err, v.Status)
	}
}
