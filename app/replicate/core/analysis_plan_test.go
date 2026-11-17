package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

const visionJSON = `{"type":"product","purpose":"sell","audio_kind":"speech","audio_note":"女声口播",
 "dims":{"model":{"present":true,"desc":"长发女生","risk":"high"},"product":{"present":true,"desc":"白色运动鞋","risk":"mid"},
         "garment":{"present":true,"desc":"黑色外套","risk":"mid","occurrences":[{"t0":0,"t1":5,"desc":"黑外套"},{"t0":5,"t1":99,"desc":"白T"}]},
         "accessory":{"present":false,"desc":"不应保留"}},
 "dim_order":["product","model","bogus"],
 "subjects":[{"id":"p1","name":"女主","desc":"长发"}],
 "sections":[{"t0":0,"t1":4.2,"desc":"展示鞋","action":"模特展示外套"},{"t0":4,"t1":8,"desc":"近景","action":"近景细节"},{"t0":8,"t1":120,"desc":"走动","action":"全身走动"}],
 "on_screen_text":[],"lines":[]}`

func setupAnalysisGW(e *env) {
	e.gw.analysis["chat.completion"] = json.RawMessage(`{"operation":"chat.completion","message":{"content":"` + "```json\\n" + jsonEscape(visionJSON) + "\\n```" + `"}}`)
	e.gw.analysis["asr.transcribe"] = json.RawMessage(`{"operation":"asr.transcribe","text":"这双鞋很轻","sentences":[{"index":0,"begin_ms":500,"end_ms":2100,"text":"这双鞋"},{"index":1,"begin_ms":3000,"end_ms":4000,"text":"很轻"}]}`)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func (e *env) drainAnalysis(id int64) {
	for i := 0; i < 20; i++ {
		more, err := e.svc.AdvanceAnalysis(e.ctx, id)
		if err != nil {
			e.t.Fatal(err)
		}
		if !more {
			return
		}
	}
	e.t.Fatal("分析没有收敛")
}

func TestAnalysisHappyPath(t *testing.T) {
	e := newEnv(t, false)
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 12.4, Width: 720, Height: 1280, Fps: 30, HasAudio: true}
	setupAnalysisGW(e)
	v, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://own.test/aigc/video/7/20261113/v.mp4?sign=abc"})
	if err != nil || v.Status != "queued" || v.Stages[0].Status != "active" || v.FreeText == "" {
		t.Fatalf("%v %+v", err, v)
	}
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	v, _ = e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	if v.Status != "succeeded" || v.Result == nil {
		t.Fatalf("应成功：%+v", v)
	}
	for _, s := range v.Stages {
		if s.Status != "done" {
			t.Fatalf("四个阶段都应完成：%+v", v.Stages)
		}
	}
	r := v.Result
	if r.Dims["accessory"].Present || r.Dims["accessory"].Desc != "" {
		t.Fatal("present=false 的项描述应清空")
	}
	if r.DimOrder[0] != "product" || len(r.DimOrder) != 11 {
		t.Fatalf("排序应保留模型给的合法键并补齐：%v", r.DimOrder)
	}
	if n := len(r.Sections); n != 3 || r.Sections[1].T0 != 4.2 || r.Sections[2].T1 != 12.4 {
		t.Fatalf("分镜应去重叠并夹到时长：%+v", r.Sections)
	}
	if len(r.Lines) != 2 || !r.Dims["line"].Present {
		t.Fatalf("台词以转写为准：%+v", r.Lines)
	}
	if r.Summary != "看懂了：3 个镜头，模特展示外套、近景细节、全身走动；有人声讲解；画面上没有字幕" {
		t.Fatalf("概述：%s", r.Summary)
	}
	if e.gw.submitted(":vision") != 1 || e.gw.submitted(":transcribe") != 1 {
		t.Fatalf("网关调用：%v", e.gw.submits)
	}
	if !strings.HasPrefix(e.gw.submits[0], "rpl-a-") {
		t.Fatalf("幂等键格式：%v", e.gw.submits)
	}
	msgs := e.gw.reqs[e.gw.submits[1]].Input.(map[string]any)["messages"].([]map[string]any)
	if !strings.Contains(msgs[0]["content"].([]map[string]any)[0]["text"].(string), "十一维") {
		t.Fatal("应带系统提示词")
	}
	// 同一视频（签名不同）再提交：命中缓存，不再分析
	v2, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://own.test/aigc/video/7/20261113/v.mp4?sign=zzz"})
	if v2.AnalysisNo != v.AnalysisNo || !v2.Cached || v2.CachedText == "" || e.media.ingests != 1 {
		t.Fatalf("应命中缓存：%+v", v2)
	}
	// 别人看不到
	if _, err := e.svc.GetAnalysis(e.ctx, uid+1, v.AnalysisNo); err == nil {
		t.Fatal("越权应找不到")
	}
}

func TestAnalysisRejectsTooLongAndRestart(t *testing.T) {
	e := newEnv(t, false)
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 200, HasAudio: true}
	v, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("abc")})
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	v, _ = e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	if v.Status != "failed" || v.Error != "这条视频有 3 分 20 秒，单次最长支持 80 秒。可以先剪出最精彩的一段（比如开头 80 秒内），再上传" {
		t.Fatalf("超长应拒收：%+v", v)
	}
	// 再提交同一条：重新开始（attempt+1）
	e.media.probe.DurationS = 9
	setupAnalysisGW(e)
	v, _ = e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("abc")})
	if v.Status != "queued" {
		t.Fatalf("失败后应可重新分析：%+v", v)
	}
	e.drainAnalysis(a.ID)
	a, _ = e.store.GetAnalysis(e.ctx, a.ID)
	if a.Status != model.AnalysisSucceeded || a.Attempt != 1 {
		t.Fatalf("%+v", a)
	}
	// 第三方 https 地址（不是本站存储签发）：1003 拒收，不交给媒体服务
	if _, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://v.douyin.com/abc"}); !isParamErr(err) {
		t.Fatalf("第三方地址应拒收：%v", err)
	}
	if _, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "7.84 复制打开抖音"}); err == nil {
		t.Fatal("粘贴的分享口令应拒绝")
	}
}

func TestAnalysisSilentSkipsTranscribeAndDegradesSheet(t *testing.T) {
	e := newEnv(t, false)
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 3, Width: 480, Height: 854, HasAudio: false}
	e.media.gridErr = &mediasvc.Error{Status: 400, Code: "bad"}
	setupAnalysisGW(e)
	v, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("silent")})
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	v, _ = e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	if v.Status != "succeeded" || e.gw.submitted(":transcribe") != 0 || v.Stages[2].Status != "skipped" {
		t.Fatalf("无声应跳过台词：%+v", v)
	}
	want := []string{"原视频只有 3 秒，生成最短按 4 秒计，会比原片多 1 秒", "原片清晰度偏低（480p），生成的视频不会比原片更清楚", "原片没有声音，成片会重新配乐或保持无声"}
	if strings.Join(v.Warnings, "|") != strings.Join(want, "|") {
		t.Fatalf("告警：%v", v.Warnings)
	}
	if v.Result.AudioKind != "silent" {
		t.Fatal("无音轨强制 silent")
	}
}

func TestAnalysisVisionFailureFails(t *testing.T) {
	e := newEnv(t, false)
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 10, HasAudio: false}
	e.gw.analysis["chat.completion"] = json.RawMessage(`{"operation":"chat.completion","message":{"content":"抱歉我看不懂"}}`)
	v, _ := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("x")})
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	v, _ = e.svc.GetAnalysis(e.ctx, uid, v.AnalysisNo)
	if v.Status != "failed" || !strings.Contains(v.Error, "重新试一次") {
		t.Fatalf("%+v", v)
	}
}

func TestPlanGaps(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(10)
	r := AnalysisResult{}
	_ = json.Unmarshal(a.Result, &r)
	r.Dims["accessory"] = Dim{Present: false}
	b, _ := json.Marshal(r)
	e.store.analyses[0].Result = b
	_, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.nope",
		Annotations: Annotations{Dims: map[string]DimEdit{
			"product": {Mode: ModeReplace}, "scene": {Mode: ModeDescribe}, "accessory": {Mode: ModeReplace, Refs: []int{3}}}}})
	var ge *GapsError
	if !errors.As(err, &ge) {
		t.Fatalf("应返回缺项：%v", err)
	}
	codes := map[string]string{}
	for _, g := range ge.Gaps {
		codes[g.Field+"/"+g.Code] = g.Message
	}
	for _, k := range []string{"modelGroup/MODEL_UNKNOWN", "dims.product/REF_MISSING", "dims.scene/NOTE_MISSING", "dims.accessory/DIM_ABSENT", "dims.accessory/REF_UNKNOWN"} {
		if _, ok := codes[k]; !ok {
			t.Fatalf("缺 %s：%v", k, codes)
		}
	}
	if codes["dims.product/REF_MISSING"] != "要把商品换掉，需要一张你的商品图。放一张正面清晰的就行" {
		t.Fatal(codes)
	}
	// 什么都没改
	_, err = e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.wan-3.0"})
	if !errors.As(err, &ge) || ge.Gaps[0].Code != "NOTHING_CHANGED" {
		t.Fatalf("%v", err)
	}
}

func TestPlanSingleFlightAndHash(t *testing.T) {
	e := newEnv(t, true)
	a := e.seedAnalysis(32, 12, 25)
	p1 := e.plan(a)
	p2 := e.plan(a)
	if p1.PlanNo != p2.PlanNo || p1.PlanHash != p2.PlanHash {
		t.Fatal("同输入应同方案")
	}
	p3, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Audio: AudioKeep, Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil || p3.PlanHash == p1.PlanHash {
		t.Fatalf("改了声音应是新方案：%v", err)
	}
	if len(p1.Timeline.Segments) != 3 || p1.Timeline.Segments[0].CutLabel != "镜头切换处" || p1.Timeline.Single {
		t.Fatalf("时间线：%+v", p1.Timeline)
	}
	if p1.Overview != "换商品 · 重新配乐 · 共 32 秒 · 3 段" {
		t.Fatalf("概述：%s", p1.Overview)
	}
	if p1.Cost.First != 120 || p1.Cost.Rest != 220 || p1.Cost.Total != 340 {
		t.Fatalf("报价（12/14/8 秒 × 10）：%+v", p1.Cost.Stages)
	}
	if p1.Cost.FirstText != "现在先做第 1 段：120 点" || !strings.Contains(p1.Cost.RestText, "剩下 2 段：约 220 点") || p1.Cost.OverlapText == "" {
		t.Fatalf("费用块：%+v", p1.Cost)
	}
	if !strings.Contains(p1.Prompts[1], "@图片1") || !strings.Contains(p1.Prompts[1], "第 2 段") || !strings.Contains(p1.Prompts[1], "不要生成任何文字") {
		t.Fatalf("提示词：%s", p1.Prompts[1])
	}
	if len(p3.Risks) == 0 || !strings.Contains(strings.Join(p3.Risks, ""), "保留原视频的声音") {
		t.Fatalf("保留原声应有风险提示：%v", p3.Risks)
	}
	if p1.StageQuotes != (StageQuotes{First: 120, Rest: 220, Total: 340}) || p1.ETA.First == nil || p1.ETA.Total.Text != "预计 4–6 分钟" || !p1.ETA.Prior {
		t.Fatalf("stageQuotes / eta：%+v %+v", p1.StageQuotes, p1.ETA)
	}
	q, err := e.svc.QuotePlan(e.ctx, uid, p1.PlanNo)
	if err != nil || q.Cost.Total != 340 || q.Cost.ValidUntil.Sub(e.clock).Minutes() != 10 {
		t.Fatalf("重新报价：%v %+v", err, q.Cost)
	}
}

func TestPlanLongformNeedsCutter(t *testing.T) {
	e := newEnv(t, false)
	_, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: e.seedAnalysis(32, 12).AnalysisNo, ModelGroup: "vid.seedance-2.5",
		RefImages: []string{testRef}, Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if code, _, _ := xerr.Parse(err); code != xerr.NotReady {
		t.Fatalf("没有区间切段时多段不开放：%v", err)
	}
	if e.svc.Limits().LongformEnabled || e.svc.Limits().MaxDurationSec != 80 {
		t.Fatal("limits 应如实反映")
	}
}

// ---- BillingLedger 适配器 ----

type fakeBilling struct {
	status   string
	held     *billing.Reservation
	settle   error
	refunded int
	quoteErr error
}

func (f *fakeBilling) Quote(context.Context, billing.Charge) (*billing.Quote, error) {
	if f.quoteErr != nil {
		return nil, f.quoteErr
	}
	return &billing.Quote{PricePoints: 150}, nil
}
func (f *fakeBilling) Hold(context.Context, billing.Charge) (*billing.Reservation, error) {
	return f.held, nil
}
func (f *fakeBilling) Status(context.Context, int64, string) (string, error) { return f.status, nil }
func (f *fakeBilling) Load(context.Context, int64, string) (*billing.Reservation, error) {
	if f.status == resHeld {
		return &billing.Reservation{No: "BR1"}, nil
	}
	return nil, nil
}
func (f *fakeBilling) Refund(context.Context, *billing.Reservation, string) error {
	f.refunded++
	return nil
}
func (f *fakeBilling) Settle(context.Context, int64, string, int) error { return f.settle }
func (f *fakeBilling) Renew(context.Context, int64, string) error       { return nil }

func isParamErr(err error) bool {
	code, _, _ := xerr.Parse(err)
	return code == xerr.ParamError
}

func TestBillingLedgerAdapter(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBilling{}
	l := &BillingLedger{b: fb}
	if p, err := l.Quote(ctx, billing.Charge{}); p != 150 || err != nil {
		t.Fatal(p, err)
	}
	fb.quoteErr = billing.ErrNoOffer
	if _, err := l.Quote(ctx, billing.Charge{EntryCode: EntryCode}); err == nil || !strings.Contains(err.Error(), "暂未配置价格") {
		t.Fatalf("未配价应翻译成用户文案：%v", err)
	}
	// Hold：off/shadow 没有单
	if held, err := l.Hold(ctx, billing.Charge{}); held || err != nil {
		t.Fatal(held, err)
	}
	// Hold 命中已退回的旧单：不能当成预留成功
	fb.held, fb.status = &billing.Reservation{No: "BR1"}, resRefunded
	if _, err := l.Hold(ctx, billing.Charge{RequestID: "x"}); !errors.Is(err, ErrChargeGone) {
		t.Fatalf("旧单不能当新预留：%v", err)
	}
	fb.status = resHeld
	if held, err := l.Hold(ctx, billing.Charge{RequestID: "x"}); !held || err != nil {
		t.Fatal(held, err)
	}
	// Refund 各状态
	cases := []struct {
		status   string
		refunded bool
		err      error
		calls    int
	}{{"", false, nil, 0}, {resHeld, true, nil, 1}, {resRefunded, true, nil, 0}, {resExpired, true, nil, 0}, {resConfirmed, false, ErrChargeConfirmed, 0}}
	for _, c := range cases {
		fb.status, fb.refunded = c.status, 0
		ok, err := l.Refund(ctx, 1, "x", "r")
		if ok != c.refunded || !errors.Is(err, c.err) || fb.refunded != c.calls {
			t.Fatalf("status=%s ok=%v err=%v calls=%d", c.status, ok, err, fb.refunded)
		}
	}
	fb.settle = billing.ErrGone
	if err := l.Confirm(ctx, 1, "x"); !errors.Is(err, ErrChargeGone) {
		t.Fatalf("预留失效应映射 ErrChargeGone：%v", err)
	}
}

// 失败分类：只对瞬时错误自动重试。
func TestClassify(t *testing.T) {
	cases := map[string]bool{"VENDOR_FAILED": true, "NO_CAPACITY": true, "DEADLINE_EXCEEDED": true, "CONTENT_POLICY": false, "INPUT_REJECTED": false, "VENDOR_OUTCOME_UNKNOWN": false}
	for class, transient := range cases {
		f := classifyCall(&aigateway.Call{Status: aigateway.StatusFailed, Failure: &aigateway.Failure{Class: class}})
		if f.Transient != transient || f.Text == "" {
			t.Fatalf("%s: %+v", class, f)
		}
	}
	if f := classifyCall(&aigateway.Call{Status: aigateway.StatusUnknown}); f.Transient {
		t.Fatal("UNKNOWN 不重试")
	}
	if _, temp := classifySubmit(&aigateway.Error{HTTPStatus: 409, Code: aigateway.CodeIdempotencyConflict}); temp {
		t.Fatal("幂等冲突绝不能重试")
	}
	if MoneyText(model.ChargeSettleFailed, 100) != "退点正在处理中，一般 5 分钟内到账" || MoneyText(model.ChargeRefunded, 100) != "已退回 100 点" {
		t.Fatal("钱的文案")
	}
}
