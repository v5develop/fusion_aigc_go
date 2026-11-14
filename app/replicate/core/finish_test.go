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
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
)

type fakeComposer struct {
	cuts     []string
	concats  [][]mediasvc.ConcatItem
	overlays [][]mediasvc.TextSlot
	failN    int // 前 N 次拼接失败
}

func (c *fakeComposer) Cut(_ context.Context, src string, t0, t1 float64, idem string) (string, error) {
	c.cuts = append(c.cuts, fmt.Sprintf("%s@%.1f-%.1f", src, t0, t1))
	return fmt.Sprintf("trim(%s)", src), nil
}

func (c *fakeComposer) ConcatItems(_ context.Context, items []mediasvc.ConcatItem, _ mediasvc.ConcatParams, idem string) (mediasvc.ConcatResult, error) {
	if c.failN > 0 {
		c.failN--
		return mediasvc.ConcatResult{}, errors.New("media 503")
	}
	c.concats = append(c.concats, items)
	return mediasvc.ConcatResult{Resource: mediasvc.Resource{RID: fmt.Sprintf("final-%d", len(c.concats))}}, nil
}

func (c *fakeComposer) OverlayText(_ context.Context, rid string, slots []mediasvc.TextSlot, idem string) (mediasvc.Resource, error) {
	c.overlays = append(c.overlays, slots)
	return mediasvc.Resource{RID: "text(" + rid + ")"}, nil
}

// withComposer 在 newEnv 的场景上换一个装了合成器（与字体）的内核。
func withComposer(e *env, font string) *fakeComposer {
	fc := &fakeComposer{}
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	cfg.FontRid = font
	opts := []Option{WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }), WithOwner("test-owner"), WithComposer(fc),
		WithURLOwner(testOwner), WithSigner(e.signer)}
	if e.cutter != nil {
		opts = append(opts, WithCutter(e.cutter))
	}
	e.svc = New(cfg, e.store, e.ledger, opts...)
	return fc
}

func resultDoc(t *testing.T, e *env, jobID int64) ResultDoc {
	t.Helper()
	j, _ := e.store.GetJob(e.ctx, jobID)
	var d ResultDoc
	if err := json.Unmarshal(j.ResultDocument, &d); err != nil {
		t.Fatalf("result_document: %s %v", j.ResultDocument, err)
	}
	return d
}

func (e *env) planWithTexts(a *model.Analysis, texts []TextOverlay) *PlanView {
	e.t.Helper()
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}, Texts: texts}})
	if err != nil {
		e.t.Fatalf("CreatePlan: %v", err)
	}
	return pv
}

func TestFinalizeSingleSegmentNoMediaCall(t *testing.T) {
	e := newEnv(t, false)
	fc := withComposer(e, "")
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(12.3)), "", "k1").JobID, 3)
	if jv.Status != "succeeded" || jv.Result == nil || jv.Result.Status != ResultReady || jv.Result.VideoRid != jv.Segments[0].VideoRid {
		t.Fatalf("单段无叠字：成片就是该段：%+v", jv.Result)
	}
	if len(fc.cuts)+len(fc.concats)+len(fc.overlays) != 0 {
		t.Fatal("单段无叠字不应调媒体服务")
	}
}

func TestFinalizeMultiSegmentTrimConcatAndText(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "font-rid")
	pv := e.planWithTexts(e.seedAnalysis(32, 12, 25), []TextOverlay{{Text: " 悦足堂 ", Kind: "shop_name"}})
	jv := e.run(e.confirm(pv, "all", "k1").JobID, 6)
	if jv.Status != "succeeded" || jv.Result == nil || jv.Result.Status != ResultReady || jv.Result.Kind != ResultFull {
		t.Fatalf("应合成完整成片：%+v %+v", jv.Status, jv.Result)
	}
	if len(fc.concats) != 1 || len(fc.concats[0]) != 3 {
		t.Fatalf("三段拼接：%+v", fc.concats)
	}
	if len(fc.cuts) < 2 {
		t.Fatalf("第 2、3 段要去掉衔接重叠：%v", fc.cuts)
	}
	if len(fc.overlays) != 1 || fc.overlays[0][0].Text != "悦足堂" || fc.overlays[0][0].FontRid != "font-rid" || jv.Result.VideoRid != "text(final-1)" {
		t.Fatalf("店名叠加：%+v %s", fc.overlays, jv.Result.VideoRid)
	}
	// 依据不变：再推进不重做
	e.run(jv.JobID, 2)
	if len(fc.concats) != 1 {
		t.Fatal("依据不变不应重新合成")
	}
}

func TestFinalizePreviewPlaceholderThenRedoReplaces(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.confirm(pv, "all", "k1")
	e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/02/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "partial" || jv.Result == nil || jv.Result.Kind != ResultPreview || len(jv.Result.Missing) != 1 || jv.Result.Missing[0] != 2 {
		t.Fatalf("部分成功应出占位预览版：%+v", jv.Result)
	}
	ph := fc.concats[0][1].Placeholder
	if ph == nil || !strings.Contains(ph.Text, "Segment 2") || ph.FontRid != "" || ph.Duration <= 0 {
		t.Fatalf("没配字体：英文占位：%+v", ph)
	}
	if d := resultDoc(t, e, jv.JobID); len(d.Warnings) == 0 || d.Warnings[0] != "PLACEHOLDER_NO_FONT" {
		t.Fatalf("应记 WARN：%+v", d)
	}
	// 重做成功 → 依据变化 → 重新合成完整成片
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1"}); err != nil {
		t.Fatal(err)
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "succeeded" || jv.Result.Kind != ResultFull || len(fc.concats) != 2 || fc.concats[1][1].Placeholder != nil {
		t.Fatalf("重做成功后应换成完整成片：%+v %+v", jv.Status, jv.Result)
	}
}

func TestFinalizeChinesePlaceholderWithFont(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "font-rid")
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
	e.gw.script["seg/03/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/03/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.run(jv.JobID, 6)
	if ph := fc.concats[0][2].Placeholder; ph == nil || ph.Text != DefaultPlaceholderText || ph.FontRid != "font-rid" {
		t.Fatalf("配了字体：中文占位：%+v", ph)
	}
}

func TestFinalizeRetryThenGiveUp(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "")
	fc.failN = 10
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1").JobID, 6)
	if jv.Status != "succeeded" {
		t.Fatalf("合成失败不影响任务结果与钱：%s", jv.Status)
	}
	e.assertMoney(jv.JobID)
	for i := 0; i < 5; i++ {
		e.run(jv.JobID, 1)
	}
	d := resultDoc(t, e, jv.JobID)
	if d.Status != ResultFailed || d.Attempts != resultMaxAttempts {
		t.Fatalf("重试 %d 次后放弃：%+v", resultMaxAttempts, d)
	}
	if v := resultView(mustJSON(d)); v.VideoRid != "" || v.Text == "" {
		t.Fatalf("放弃后提示下载逐段：%+v", v)
	}
}

func TestPlanTextValidationGaps(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(12.3)
	long := strings.Repeat("字", 31)
	_, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}},
			Texts: []TextOverlay{{Text: long}, {Text: ""}, {Text: "a", Box: &mediasvc.TextBox{X: 0.5, Y: 0.5, W: 0.6, H: 0.1}}}}})
	var ge *GapsError
	if !errors.As(err, &ge) {
		t.Fatalf("应返回缺项：%v", err)
	}
	codes := map[string]string{}
	for _, g := range ge.Gaps {
		codes[g.Code] = g.Field
	}
	if codes["TEXT_TOO_LONG"] != "texts[0].text" || codes["TEXT_EMPTY"] != "texts[1].text" || codes["TEXT_BOX"] != "texts[2].box" {
		t.Fatalf("缺项码：%+v", ge.Gaps)
	}
	// 只加店名也算有改动
	if _, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5",
		Annotations: Annotations{Texts: []TextOverlay{{Text: "悦足堂"}}}}); err != nil {
		t.Fatalf("只叠店名应可生成方案：%v", err)
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
