package replicateagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/handler/internalauth"
	"aigc-local-life/app/replicate/agenttool"
	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/httpapi"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// ---- 替身：记录每次调用的方法与参数 ----

type fakeSession struct {
	calls []string
	got   map[string]any
	// 可注入的结果 / 错误
	err      error
	job      *core.JobView
	curPlan  *core.PlanView
	preview  *core.PreviewView
	patchReq core.PatchReq
	// batchReplay ConfirmBatch 返回的批次是否标记为重放
	batchReplay bool
	copyReplay  bool
	// origins 确认类调用收到的来源（ctx 里的 origin，没有为 ""）
	origins []string
}

func (f *fakeSession) noteOrigin(ctx context.Context) {
	if o, ok := core.OriginOf(ctx); ok {
		f.origins = append(f.origins, o.Kind+"|"+o.ConversationID+"|"+o.TurnID)
		return
	}
	f.origins = append(f.origins, "")
}

func (f *fakeSession) rec(name string, arg any) {
	f.calls = append(f.calls, name)
	if f.got == nil {
		f.got = map[string]any{}
	}
	f.got[name] = arg
}

func (f *fakeSession) jobOr() *core.JobView {
	if f.job != nil {
		return f.job
	}
	return &core.JobView{JobID: 42, Status: "running"}
}
func (f *fakeSession) Limits() httpapi.LimitsView {
	f.rec("Limits", nil)
	return httpapi.LimitsView{Enabled: true}
}
func (f *fakeSession) CreateAnalysis(_ context.Context, u string) (*core.AnalysisView, error) {
	f.rec("CreateAnalysis", u)
	return &core.AnalysisView{AnalysisNo: "VRA1"}, f.err
}
func (f *fakeSession) GetAnalysis(_ context.Context, n string) (*core.AnalysisView, error) {
	f.rec("GetAnalysis", n)
	return &core.AnalysisView{AnalysisNo: n}, f.err
}
func (f *fakeSession) CreatePlan(_ context.Context, r core.PlanReq) (*core.PlanView, error) {
	f.rec("CreatePlan", r)
	return &core.PlanView{PlanNo: "VRP1"}, f.err
}
func (f *fakeSession) QuotePlan(_ context.Context, n string) (*core.PlanView, error) {
	f.rec("QuotePlan", n)
	return &core.PlanView{PlanNo: n}, f.err
}
func (f *fakeSession) Confirm(ctx context.Context, a httpapi.ConfirmArgs) (*core.JobView, error) {
	f.noteOrigin(ctx)
	f.rec("Confirm", a)
	return f.jobOr(), f.err
}
func (f *fakeSession) JobView(_ context.Context, id int64) (*core.JobView, error) {
	f.rec("JobView", id)
	return f.jobOr(), f.err
}
func (f *fakeSession) ListJobs(_ context.Context, p, s int) (*core.JobList, error) {
	f.rec("ListJobs", [2]int{p, s})
	return &core.JobList{}, f.err
}
func (f *fakeSession) Continue(_ context.Context, a httpapi.ContinueArgs) (*core.JobView, error) {
	f.rec("Continue", a)
	return f.jobOr(), f.err
}
func (f *fakeSession) Stop(_ context.Context, id int64) (*core.JobView, error) {
	f.rec("Stop", id)
	return f.jobOr(), f.err
}
func (f *fakeSession) CancelRest(_ context.Context, id int64) (*core.JobView, error) {
	f.rec("CancelRest", id)
	return f.jobOr(), f.err
}
func (f *fakeSession) Models(_ context.Context, r core.ModelsReq) (*core.ModelList, error) {
	f.rec("Models", r)
	return &core.ModelList{}, f.err
}
func (f *fakeSession) Redo(_ context.Context, a httpapi.RedoArgs) (*core.JobView, error) {
	f.rec("Redo", a)
	return f.jobOr(), f.err
}
func (f *fakeSession) ListSamples(_ context.Context, i string) (*core.SampleList, error) {
	f.rec("ListSamples", i)
	return &core.SampleList{}, f.err
}
func (f *fakeSession) TrySample(_ context.Context, id int64) (*core.SampleTry, error) {
	f.rec("TrySample", id)
	return &core.SampleTry{}, f.err
}
func (f *fakeSession) GetPlan(_ context.Context, n string) (*core.PlanView, error) {
	f.rec("GetPlan", n)
	if f.curPlan != nil {
		return f.curPlan, f.err
	}
	return &core.PlanView{PlanNo: n}, f.err
}
func (f *fakeSession) PreviewFrame(_ context.Context, n string) (*core.PreviewView, error) {
	f.rec("PreviewFrame", n)
	if f.preview != nil {
		return f.preview, f.err
	}
	return &core.PreviewView{PlanNo: n}, f.err
}
func (f *fakeSession) PatchAnnotations(_ context.Context, r core.PatchReq, has bool) (*core.PlanView, error) {
	f.rec("PatchAnnotations", [2]any{r, has})
	f.patchReq = r
	return &core.PlanView{PlanNo: "VRP2"}, f.err
}

func (f *fakeSession) QuoteBatch(_ context.Context, no string, vs []httpapi.BatchVariantArgs) (*core.BatchQuoteView, error) {
	f.rec("QuoteBatch", [2]any{no, vs})
	return &core.BatchQuoteView{PlanNo: no, Count: len(vs), Total: 100}, f.err
}
func (f *fakeSession) ConfirmBatch(ctx context.Context, a httpapi.BatchConfirmArgs) (*core.BatchView, error) {
	f.noteOrigin(ctx)
	f.rec("ConfirmBatch", a)
	return &core.BatchView{BatchID: 7, PlanNo: a.PlanNo, Status: "running", Replay: f.batchReplay}, f.err
}
func (f *fakeSession) GetBatch(_ context.Context, id int64) (*core.BatchView, error) {
	f.rec("GetBatch", id)
	return &core.BatchView{BatchID: id}, f.err
}

func (f *fakeSession) PublishCopy(_ context.Context, jobID int64, platform string) (*core.PublishCopyView, error) {
	f.rec("PublishCopy", [2]any{jobID, platform})
	return &core.PublishCopyView{JobID: jobID, Platform: platform, Replay: f.copyReplay}, f.err
}
func (f *fakeSession) ExportRatio(_ context.Context, jobID int64, ratio, mode string) (*core.ExportView, error) {
	f.rec("ExportRatio", [3]any{jobID, ratio, mode})
	return &core.ExportView{JobID: jobID, Ratio: ratio, Mode: mode}, f.err
}

func (f *fakeSession) RetryAudio(_ context.Context, jobID int64, callID string) (*core.AudioRetryView, error) {
	f.rec("RetryAudio", [2]any{jobID, callID})
	return &core.AudioRetryView{JobID: jobID, Audio: core.AudioResultKept, Replay: f.copyReplay}, f.err
}

func (f *fakeSession) ListVoices(_ context.Context, gender, style string) (*core.VoiceList, error) {
	f.rec("ListVoices", [2]string{gender, style})
	return &core.VoiceList{Voices: []core.VoiceView{{VoiceID: 3, Name: "小云", Gender: "female"}}}, f.err
}

func (f *fakeSession) Trending(_ context.Context, industry string) (*core.TrendingView, error) {
	f.rec("Trending", industry)
	return &core.TrendingView{Industry: industry, Period: "7d", Items: []core.TrendingItem{{Label: "换模特 + 保留原声"}}}, f.err
}

func (f *fakeSession) ShareSample(_ context.Context, jobID int64, consent bool) (*core.ShareView, error) {
	f.rec("ShareSample", [2]any{jobID, consent})
	return &core.ShareView{SubmissionID: 5, JobID: jobID, Status: "pending", Replay: f.copyReplay}, f.err
}

func (f *fakeSession) WithdrawSample(_ context.Context, id int64) (*core.WithdrawView, error) {
	f.rec("WithdrawSample", id)
	return &core.WithdrawView{SubmissionID: id, Status: "withdrawn", Unpublished: true, Replay: f.copyReplay}, f.err
}

func (f *fakeSession) SubmitFeedback(_ context.Context, r core.FeedbackReq) (*core.FeedbackView, error) {
	f.rec("SubmitFeedback", r)
	return &core.FeedbackView{JobID: r.JobID, Rating: r.Rating, Message: "谢谢，已记录"}, f.err
}

type fakeBackend struct {
	s        *fakeSession
	enterErr error
	entered  int
	uid      int64
	write    bool
}

func (b *fakeBackend) Enter(_ context.Context, uid int64, write bool) (Session, error) {
	b.entered++
	b.uid, b.write = uid, write
	if b.enterErr != nil {
		return nil, b.enterErr
	}
	return b.s, nil
}

func newBackend() *fakeBackend { return &fakeBackend{s: &fakeSession{}} }

// ---- 链路：与 gateway.go 同序（PreserveInternalBody → jsoncase → 路由），验签用同一个共享校验器 ----

func chain(t *testing.T, be Backend) (call, tools http.HandlerFunc) {
	t.Helper()
	result.Setup()
	var cfg config.CanvasInternalConf
	cfg.Keys = []config.CanvasInternalKey{{KeyId: "java-1", Secret: testSecret}}
	cfg.MaxSkewSeconds, cfg.NonceTTLSeconds = 300, 600
	v := internalauth.BuildVerifier(cfg, svcauth.NewMemoryNonces())
	if v == nil {
		t.Fatal("应建出校验器")
	}
	call = internalauth.PreserveInternalBody(jsoncase.Middleware(CallHandler(be, v)))
	tools = internalauth.PreserveInternalBody(jsoncase.Middleware(ToolsHandler(v)))
	return call, tools
}

var nonceSeq int

func signed(body string, uid int64) *http.Request {
	nonceSeq++
	r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	svcauth.SignRequest(r, "java-1", testSecret, uid, fmt.Sprintf("nonce-replicate-%04d", nonceSeq), time.Now(), []byte(body))
	return r
}

type envelope struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Data    struct {
		Name     string         `json:"name"`
		Tool     string         `json:"tool"`
		CallID   string         `json:"call_id"`
		IsError  bool           `json:"is_error"`
		Replayed bool           `json:"replayed"`
		Result   map[string]any `json:"result"`
		Content  string         `json:"content"`
	} `json:"data"`
}

func do(t *testing.T, h http.HandlerFunc, r *http.Request) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, r)
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("响应不是 JSON：%s", rec.Body.String())
	}
	return rec, e
}

func body(call, tool, args string) string {
	return fmt.Sprintf(`{"call_id":%q,"tool":%q,"arguments":%s}`, call, tool, args)
}

// ---- 验签与协议级拒绝 ----

func TestCallSignedOK(t *testing.T) {
	be := newBackend()
	call, _ := chain(t, be)
	// 驼峰键（planHash / expectedPoints）在 jsoncase 白名单里会被补 snake 别名：验签与解析必须用原始 body
	rec, e := do(t, call, signed(body("turn-1:tool-1", "replicate_confirm", `{"plan_no":"VRP1","stage":"first","plan_hash":"h1","expected_points":120}`), 42))
	if rec.Code != 200 || e.Code != 0 || e.Data.IsError {
		t.Fatalf("签名正确应执行：%d %s", rec.Code, rec.Body.String())
	}
	if be.uid != 42 || !be.write || len(be.s.calls) != 1 || be.s.calls[0] != "Confirm" {
		t.Fatalf("身份 / 写标记 / 分派不对：uid=%d write=%v calls=%v", be.uid, be.write, be.s.calls)
	}
	a := be.s.got["Confirm"].(httpapi.ConfirmArgs)
	if a.IdempotencyKey != "turn-1:tool-1" || a.PlanNo != "VRP1" || a.Stage != "first" || a.PlanHash != "h1" || a.ExpectedPoints == nil || *a.ExpectedPoints != 120 {
		t.Fatalf("确认入参 / 幂等键（= call_id）不对：%+v", a)
	}
	if e.Data.Name != "replicate_confirm" || e.Data.Tool != "replicate_confirm" || e.Data.CallID != "turn-1:tool-1" || e.Data.Content == "" {
		t.Fatalf("出参：%s", rec.Body.String())
	}
	if e.Data.Result["jobId"] != float64(42) {
		t.Fatalf("result 应是 JobView 原样（camelCase）：%v", e.Data.Result)
	}
	if strings.Contains(rec.Body.String(), `"callId"`) {
		t.Fatal("出参不应补驼峰别名")
	}
}

func TestCallRejects(t *testing.T) {
	ok := body("c-1", "replicate_limits", `{}`)
	cases := []struct {
		name   string
		req    func() *http.Request
		status int
		reason string
	}{
		{"错签", func() *http.Request {
			nonceSeq++
			r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(ok))
			svcauth.SignRequest(r, "java-1", testSecret+"x", 42, fmt.Sprintf("nonce-replicate-bad-%d", nonceSeq), time.Now(), []byte(ok))
			return r
		}, 401, svcauth.ReasonBadSignature},
		{"缺 user", func() *http.Request {
			r := signed(ok, 42)
			r.Header.Del(svcauth.HeaderUserID)
			return r
		}, 401, svcauth.ReasonUserMissing},
		{"只带用户令牌", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(ok))
			r.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
			r.Header.Set("X-User-Id", "42")
			return r
		}, 401, svcauth.ReasonMissing},
		{"请求体不是调用", func() *http.Request { return signed(`{"foo":1}`, 42) }, 400, "PARAM_INVALID"},
		{"缺 call_id", func() *http.Request { return signed(`{"tool":"replicate_limits","arguments":{}}`, 42) }, 400, "CALL_ID_REQUIRED"},
		{"缺 call_id 时 Idempotency-Key / X-Request-Id 头不顶替", func() *http.Request {
			r := signed(`{"tool":"replicate_limits","arguments":{}}`, 42)
			r.Header.Set("Idempotency-Key", "k-1")
			r.Header.Set("X-Request-Id", "k-1")
			return r
		}, 400, "CALL_ID_REQUIRED"},
		{"call_id 超长", func() *http.Request {
			return signed(body(strings.Repeat("x", 65), "replicate_limits", `{}`), 42)
		}, 400, "PARAM_INVALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			be := newBackend()
			call, _ := chain(t, be)
			rec, e := do(t, call, c.req())
			if rec.Code != c.status || e.Reason != c.reason || be.entered != 0 || e.Message == "" {
				t.Fatalf("应 %d %s 且不执行：%d %s entered=%d", c.status, c.reason, rec.Code, rec.Body.String(), be.entered)
			}
		})
	}
	t.Run("nonce 重放", func(t *testing.T) {
		be := newBackend()
		call, _ := chain(t, be)
		r1 := signed(ok, 42)
		r2 := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(ok))
		for k, v := range r1.Header {
			r2.Header[k] = v
		}
		rec, _ := do(t, call, r1)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		rec, e := do(t, call, r2)
		if rec.Code != 401 || e.Reason != svcauth.ReasonReplay || be.entered != 1 {
			t.Fatalf("重放应 401：%s", rec.Body.String())
		}
	})
}

// 未知工具 / 参数不合 Schema：HTTP 200，工具错误（is_error）；不进后端。
func TestUnknownToolAndBadArgs(t *testing.T) {
	be := newBackend()
	call, _ := chain(t, be)
	_, e := do(t, call, signed(body("c1", "replicate_nope", `{}`), 42))
	if !e.Data.IsError || e.Data.Result["error_code"] != "UNKNOWN_TOOL" || e.Data.Result["message"] == "" {
		t.Fatalf("未知工具：%+v", e.Data)
	}
	// 画布工具名也不是复刻入口的工具
	_, e = do(t, call, signed(body("c2", "canvas_list", `{}`), 42))
	if !e.Data.IsError || e.Data.Result["error_code"] != "UNKNOWN_TOOL" {
		t.Fatalf("画布工具不属于复刻入口：%+v", e.Data)
	}
	for name, args := range map[string]string{
		"缺必填":      `{}`,
		"未知字段":     `{"plan_no":"VRP1","foo":1}`,
		"stage 非法": `{"plan_no":"VRP1","stage":"half","expected_points":10}`,
		"类型不对":     `{"plan_no":1,"expected_points":10}`,
	} {
		_, e = do(t, call, signed(body("c3-"+name, "replicate_confirm", args), 42))
		if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" {
			t.Fatalf("%s 应 PARAM_INVALID：%+v", name, e.Data)
		}
	}
	if be.entered != 0 {
		t.Fatalf("校验没过不应进后端：%d", be.entered)
	}
}

// 扣点工具 expected_points 必填且 >0：缺失 / 0 / 负数都回参数错误，且在进后端（查重放 / 限频 / 动钱）之前。
func TestExpectedPointsMandatoryForChargingTools(t *testing.T) {
	tools := map[string]string{
		"replicate_confirm":      `"plan_no":"VRP1","plan_hash":"h"`,
		"replicate_continue":     `"job_id":42,"plan_hash":"h"`,
		"replicate_redo_segment": `"job_id":42,"seg_index":2`,
	}
	for tool, base := range tools {
		for _, pts := range []string{``, `,"expected_points":0`, `,"expected_points":-3`, `,"expected_points":null`} {
			be := newBackend()
			call, _ := chain(t, be)
			rec, e := do(t, call, signed(body("c-pts", tool, "{"+base+pts+"}"), 42))
			if rec.Code != 200 || !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" ||
				!strings.Contains(fmt.Sprint(e.Data.Result["message"]), "expected_points") {
				t.Fatalf("%s %q 应 PARAM_INVALID：%s", tool, pts, rec.Body.String())
			}
			if be.entered != 0 || len(be.s.calls) != 0 {
				t.Fatalf("%s %q 缺点数不得进后端", tool, pts)
			}
		}
	}
	// 给了正数才放行
	be := newBackend()
	call, _ := chain(t, be)
	if _, e := do(t, call, signed(body("c-ok", "replicate_continue", `{"job_id":42,"plan_hash":"h","expected_points":220}`), 42)); e.Data.IsError || be.entered != 1 {
		t.Fatalf("正数应放行：%+v", e.Data)
	}
}

// ---- 分派：每个工具落到对应的 Session 方法，参数与幂等键正确 ----

func TestDispatchEveryTool(t *testing.T) {
	type want struct {
		method string
		write  bool
		check  func(t *testing.T, got any)
	}
	eq := func(exp any) func(t *testing.T, got any) {
		return func(t *testing.T, got any) {
			t.Helper()
			g, _ := json.Marshal(got) // 经 JSON 比较：指针字段按值比
			w, _ := json.Marshal(exp)
			if string(g) != string(w) {
				t.Fatalf("参数：got=%s want=%s", g, w)
			}
		}
	}
	pts := func(v int64) *int64 { return &v }
	cases := []struct {
		tool, args string
		w          want
	}{
		{"replicate_limits", `{}`, want{"Limits", false, nil}},
		{"replicate_analyze", `{"video":"视频1","source":"https://x/video/7/20260101/a.mp4"}`, want{"CreateAnalysis", true, eq("https://x/video/7/20260101/a.mp4")}},
		{"replicate_get_analysis", `{"analysis_no":"VRA1"}`, want{"GetAnalysis", false, eq("VRA1")}},
		{"replicate_list_models", `{}`, want{"Models", false, nil}},
		{"replicate_list_samples", `{"industry":"food"}`, want{"ListSamples", false, eq("food")}},
		{"replicate_try_sample", `{"sample_id":3}`, want{"TrySample", false, eq(int64(3))}},
		{"replicate_get_plan", `{"plan_no":"VRP1"}`, want{"GetPlan", false, eq("VRP1")}},
		{"replicate_quote", `{"plan_no":"VRP1"}`, want{"QuotePlan", true, eq("VRP1")}},
		{"replicate_preview_frame", `{"plan_no":"VRP1"}`, want{"PreviewFrame", true, eq("VRP1")}},
		{"replicate_get_job", `{"job_id":42}`, want{"JobView", false, eq(int64(42))}},
		{"replicate_list_jobs", `{"page":2}`, want{"ListJobs", false, eq([2]int{2, 0})}},
		{"replicate_stop", `{"job_id":42}`, want{"Stop", true, eq(int64(42))}},
		{"replicate_cancel_rest", `{"job_id":42}`, want{"CancelRest", true, eq(int64(42))}},
		{"replicate_confirm", `{"plan_no":"VRP1","plan_hash":"h","stage":"all","expected_points":340}`,
			want{"Confirm", true, eq(httpapi.ConfirmArgs{PlanNo: "VRP1", PlanHash: "h", Stage: "all", ExpectedPoints: pts(340), IdempotencyKey: "call-x"})}},
		{"replicate_continue", `{"job_id":42,"plan_hash":"h","expected_points":220}`,
			want{"Continue", true, eq(httpapi.ContinueArgs{JobID: 42, PlanHash: "h", ExpectedPoints: pts(220), IdempotencyKey: "call-x"})}},
		{"replicate_redo_segment", `{"job_id":42,"seg_index":2,"note":" 动作再大一点 ","expected_points":80}`,
			want{"Redo", true, eq(httpapi.RedoArgs{JobID: 42, SegIndex: 2, Note: "动作再大一点", ExpectedPoints: pts(80), IdempotencyKey: "call-x"})}},
		{"replicate_quote_batch", `{"plan_no":"VRP1","variants":[{"ref_images_from":["图1"],"ref_images":["mr://r1"]},{"ref_images_from":["图2"],"ref_images":["mr://r2"]}]}`,
			want{"QuoteBatch", false, eq([2]any{"VRP1", []httpapi.BatchVariantArgs{{RefImages: []string{"mr://r1"}}, {RefImages: []string{"mr://r2"}}}})}},
		{"replicate_confirm_batch", `{"plan_no":"VRP1","variants":[{"ref_images_from":["图1"],"ref_images":["mr://r1"]}],"expected_points":600}`,
			want{"ConfirmBatch", true, eq(httpapi.BatchConfirmArgs{PlanNo: "VRP1", Variants: []httpapi.BatchVariantArgs{{RefImages: []string{"mr://r1"}}},
				ExpectedPoints: pts(600), IdempotencyKey: "call-x"})}},
		{"replicate_get_batch", `{"batch_id":"12"}`, want{"GetBatch", false, eq(int64(12))}},
		{"replicate_publish_copy", `{"job_id":42,"platform":"xiaohongshu"}`, want{"PublishCopy", true, eq([2]any{int64(42), "xiaohongshu"})}},
		{"replicate_publish_copy", `{"job_id":42}`, want{"PublishCopy", true, eq([2]any{int64(42), ""})}},
		{"replicate_export_ratio", `{"job_id":42,"ratio":"9:16","mode":"crop"}`, want{"ExportRatio", true, eq([3]any{int64(42), "9:16", "crop"})}},
		{"replicate_export_ratio", `{"job_id":42,"ratio":"1:1"}`, want{"ExportRatio", true, eq([3]any{int64(42), "1:1", ""})}},
		{"replicate_list_voices", `{"gender":"female","style":"亲切"}`, want{"ListVoices", false, eq([2]string{"female", "亲切"})}},
		{"replicate_retry_audio", `{"job_id":42}`, want{"RetryAudio", true, eq([2]any{int64(42), "call-x"})}},
		{"replicate_list_voices", `{}`, want{"ListVoices", false, eq([2]string{"", ""})}},
		{"replicate_trending", `{"industry":"food"}`, want{"Trending", false, eq("food")}},
		{"replicate_trending", `{}`, want{"Trending", false, eq("")}},
		{"replicate_share_sample", `{"job_id":42,"portrait_consent":true}`, want{"ShareSample", true, eq([2]any{int64(42), true})}},
		{"replicate_share_sample", `{"job_id":42}`, want{"ShareSample", true, eq([2]any{int64(42), false})}},
		{"replicate_withdraw_sample", `{"submission_id":5}`, want{"WithdrawSample", true, eq(int64(5))}},
		{"replicate_feedback", `{"job_id":42,"rating":"up"}`, want{"SubmitFeedback", false, nil}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			be := newBackend()
			call, _ := chain(t, be)
			rec, e := do(t, call, signed(body("call-x", c.tool, c.args), 42))
			if rec.Code != 200 || e.Data.IsError {
				t.Fatalf("应成功：%s", rec.Body.String())
			}
			if len(be.s.calls) != 1 || be.s.calls[0] != c.w.method || be.write != c.w.write || be.uid != 42 {
				t.Fatalf("分派：calls=%v write=%v uid=%d 期望 %s write=%v", be.s.calls, be.write, be.uid, c.w.method, c.w.write)
			}
			if c.w.check != nil {
				c.w.check(t, be.s.got[c.w.method])
			}
		})
	}
	// 每个契约工具都有分派用例
	covered := map[string]bool{"replicate_create_plan": true, "replicate_update_plan": true} // 这两个在下面单独测
	for _, c := range cases {
		covered[c.tool] = true
	}
	for _, tl := range agenttool.Tools() {
		if !covered[tl.Name] {
			t.Errorf("%s 没有分派用例", tl.Name)
		}
	}
}

func TestCreatePlanDispatch(t *testing.T) {
	be := newBackend()
	call, _ := chain(t, be)
	args := `{"analysis_no":"VRA1","annotations":{"dims":{"product":{"mode":"replace","refs":[1]},"scene":{"mode":"describe","note":"海边"}},"audio":"keep","texts":[{"text":"99元","kind":"price"}]},` +
		`"ref_images_from":["图2"],"ref_images":["https://x/image/7/20260101/p.png"],"model_group":"vid.m","resolution":"1080p","aspect_ratio":"9:16"}`
	_, e := do(t, call, signed(body("c1", "replicate_create_plan", args), 7))
	if e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	r := be.s.got["CreatePlan"].(core.PlanReq)
	if r.AnalysisNo != "VRA1" || r.ModelGroup != "vid.m" || r.Resolution != "1080p" || r.AspectRatio != "9:16" ||
		len(r.RefImages) != 1 || r.RefImages[0] != "https://x/image/7/20260101/p.png" {
		t.Fatalf("方案入参：%+v", r)
	}
	if d := r.Annotations.Dims["product"]; d.Mode != "replace" || len(d.Refs) != 1 || d.Refs[0] != 1 || r.Annotations.Dims["scene"].Note != "海边" ||
		r.Annotations.Audio != "keep" || len(r.Annotations.Texts) != 1 || r.Annotations.Texts[0].Text != "99元" {
		t.Fatalf("标注没原样带过去：%+v", r.Annotations)
	}
	// 给了序号却没有系统注入的地址：如实报错，不静默丢图
	for name, a := range map[string]string{
		"只有序号":     `{"analysis_no":"VRA1","ref_images_from":["图1"]}`,
		"序号与地址数不符": `{"analysis_no":"VRA1","ref_images_from":["图1","图2"],"ref_images":["https://x/image/7/20260101/p.png"]}`,
	} {
		be = newBackend()
		call, _ = chain(t, be)
		_, e = do(t, call, signed(body("c2-"+name, "replicate_create_plan", a), 7))
		if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || len(be.s.calls) != 0 {
			t.Fatalf("%s 应 PARAM_INVALID 且不建方案：%+v", name, e.Data)
		}
	}
}

// 换声音：annotations.audio=revoice + annotations.voice 原样带到内核；update_plan 里 voice 整体替换，audio 改成别的方式时清掉旧音色。
func TestRevoiceAnnotationsDispatch(t *testing.T) {
	be := newBackend()
	call, _ := chain(t, be)
	args := `{"analysis_no":"VRA1","annotations":{"audio":"revoice","voice":{"voiceId":5,"speed":1.1,"lines":[{"index":2,"text":"全场五折"}]}}}`
	if _, e := do(t, call, signed(body("v1", "replicate_create_plan", args), 7)); e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	a := be.s.got["CreatePlan"].(core.PlanReq).Annotations
	if a.Audio != "revoice" || a.Voice == nil || a.Voice.VoiceID != 5 || a.Voice.Speed != 1.1 || len(a.Voice.Lines) != 1 || a.Voice.Lines[0].Text != "全场五折" {
		t.Fatalf("换声音设置没原样带过去：%+v", a)
	}
	// 没有 voiceId 的 voice 被入参校验拦下
	be = newBackend()
	call, _ = chain(t, be)
	_, e := do(t, call, signed(body("v2", "replicate_create_plan", `{"analysis_no":"VRA1","annotations":{"audio":"revoice","voice":{"speed":1}}}`), 7))
	if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || len(be.s.calls) != 0 {
		t.Fatalf("缺 voiceId 应 PARAM_INVALID：%+v", e.Data)
	}
	// update：只改语速以外的维度时沿用 voice；audio 改成 keep 且没给 voice → 清掉旧音色
	cur := &core.PlanView{Annotations: &core.Annotations{Audio: "revoice", Voice: &core.VoiceEdit{VoiceID: 5}}}
	be = newBackend()
	be.s.curPlan = cur
	call, _ = chain(t, be)
	if _, e := do(t, call, signed(body("v3", "replicate_update_plan", `{"plan_no":"VRP1","annotations":{"dims":{"scene":{"mode":"describe","note":"海边"}}}}`), 7)); e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	if r := be.s.patchReq.Annotations; r.Voice == nil || r.Voice.VoiceID != 5 || r.Audio != "revoice" {
		t.Fatalf("没给 voice / audio 应沿用：%+v", r)
	}
	be = newBackend()
	be.s.curPlan = cur
	call, _ = chain(t, be)
	if _, e := do(t, call, signed(body("v4", "replicate_update_plan", `{"plan_no":"VRP1","annotations":{"audio":"keep"}}`), 7)); e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	if r := be.s.patchReq.Annotations; r.Voice != nil || r.Audio != "keep" {
		t.Fatalf("改成保留原声应清掉音色：%+v", r)
	}
	if cur.Annotations.Voice == nil {
		t.Fatal("不得改写 GetPlan 返回的原对象")
	}
}

func TestUpdatePlanMergesAnnotations(t *testing.T) {
	be := newBackend()
	be.s.curPlan = &core.PlanView{PlanNo: "VRP1", Annotations: &core.Annotations{
		Dims:  map[string]core.DimEdit{"product": {Mode: "replace", Refs: []int{1}}, "scene": {Mode: "describe", Note: "海边"}},
		Audio: "mute", Texts: []core.TextOverlay{{Text: "旧店名"}}}}
	call, _ := chain(t, be)
	// 只改场景，并加 audio；texts 没给 → 沿用；product 沿用
	_, e := do(t, call, signed(body("c1", "replicate_update_plan", `{"plan_no":"VRP1","annotations":{"dims":{"scene":{"mode":"describe","note":"雪山"}},"audio":"keep"},"model_group":"vid.n"}`), 7))
	if e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	r := be.s.patchReq
	if r.PlanNo != "VRP1" || r.ModelGroup == nil || *r.ModelGroup != "vid.n" || r.Resolution != nil || r.AspectRatio != nil || r.RefImages != nil {
		t.Fatalf("没给的字段应为 nil（沿用）：%+v", r)
	}
	if r.Annotations.Dims["scene"].Note != "雪山" || r.Annotations.Dims["product"].Mode != "replace" || r.Annotations.Audio != "keep" ||
		len(r.Annotations.Texts) != 1 || r.Annotations.Texts[0].Text != "旧店名" {
		t.Fatalf("按维度合并不对：%+v", r.Annotations)
	}
	if be.s.got["PatchAnnotations"].([2]any)[1] != true {
		t.Fatal("合并后的标注应标记为已提供")
	}
	if cur := be.s.curPlan.Annotations; cur.Dims["scene"].Note != "海边" {
		t.Fatal("不得改写 GetPlan 返回的原对象")
	}
	// texts 给了空数组 → 整体清空；ref_images_from 空数组 → 去掉所有参考图
	be = newBackend()
	be.s.curPlan = &core.PlanView{Annotations: &core.Annotations{Texts: []core.TextOverlay{{Text: "旧店名"}}}}
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("c2", "replicate_update_plan", `{"plan_no":"VRP1","annotations":{"texts":[]},"ref_images_from":[],"ref_images":[]}`), 7))
	r = be.s.patchReq
	if e.Data.IsError || len(r.Annotations.Texts) != 0 || r.RefImages == nil || len(*r.RefImages) != 0 {
		t.Fatalf("清空：%+v %+v", e.Data, r)
	}
	// 什么都没给：无事可做，不取方案、不改
	be = newBackend()
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("c3", "replicate_update_plan", `{"plan_no":"VRP1"}`), 7))
	if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || len(be.s.calls) != 0 {
		t.Fatalf("没有要改的内容应 PARAM_INVALID：%+v", e.Data)
	}
}

// ---- 错误映射 ----

func TestMapError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		code      string
		retryable bool
		retry     int
		gaps      int
		msgHas    string
	}{
		{"限频-每分钟", &httpapi.RateLimitError{Msg: "操作太快了，稍等一下再试", RetryAfter: 17, Check: "RATE_MINUTE"}, "RATE_LIMITED", true, 17, 0, "操作太快"},
		{"限频-每日不可重试", &httpapi.RateLimitError{Msg: "今天的方案次数用完了", RetryAfter: 3600, Check: "RATE_DAY"}, "RATE_LIMITED", false, 3600, 0, "今天"},
		{"限频-全局在途可重试", &httpapi.RateLimitError{Msg: "请稍等几分钟", RetryAfter: 60, Check: "RATE_GLOBAL_INFLIGHT"}, "RATE_LIMITED", true, 60, 0, "稍等"},
		{"方案缺项带 gaps", &core.GapsError{Gaps: []core.Gap{{Code: "REF_MISSING", Field: "dims.product", Message: "要一张商品图"}, {Code: "NOTE_MISSING", Field: "dims.scene", Message: "写一下场景"}}},
			"PARAM_INVALID", false, 0, 2, "要一张商品图"},
		{"价变", xerr.NewReason(xerr.Conflict, "QUOTE_CHANGED", "价格变了，现在是 130 点"), "QUOTE_CHANGED", false, 0, 0, "130"},
		{"方案已更新", xerr.NewReason(xerr.Conflict, "PLAN_STALE", "方案刚更新了"), "PLAN_STALE", false, 0, 0, "更新"},
		{"方案过期", xerr.NewReason(xerr.Conflict, "PLAN_EXPIRED", "方案过期了"), "PLAN_EXPIRED", false, 0, 0, "过期"},
		{"不能重做", xerr.NewReason(xerr.Conflict, "NOT_REDOABLE", "这个任务现在不能重做"), "NOT_REDOABLE", false, 0, 0, "不能重做"},
		{"内容检查没出结论可重试", xerr.NewReason(xerr.Conflict, "MODERATION_PENDING", "几秒后再点确认"), "MODERATION_PENDING", true, 0, 0, "几秒"},
		{"内容检查不通过", xerr.NewReason(xerr.ParamError, "MODERATION_REJECTED", "填写的文字没有通过内容检查"), "MODERATION_REJECTED", false, 0, 0, "内容检查"},
		{"定妆图额度用完不可重试", xerr.NewReason(xerr.Conflict, "PREVIEW_QUOTA_EXHAUSTED", "今天的 3 次免费定妆图已经用完了，明天 0 点恢复"), "PREVIEW_QUOTA_EXHAUSTED", false, 0, 0, "明天"},
		{"定妆图出图失败可重试", xerr.NewReason(xerr.ThirdPartyError, "THIRD_PARTY_ERROR", "定妆图这次没出成"), "THIRD_PARTY_ERROR", true, 0, 0, "没出成"},
		{"定妆图正在生成可重试", xerr.NewReason(xerr.Conflict, "IN_PROGRESS", "这张定妆图正在生成"), "IN_PROGRESS", true, 0, 0, "正在生成"},
		{"功能未开放", xerr.New(xerr.NotReady, "功能暂未开放"), "FEATURE_NOT_READY", false, 0, 0, "暂未开放"},
		{"缺参数归并到 PARAM_INVALID", xerr.New(xerr.ParamMissing, "缺少确认点数"), "PARAM_INVALID", false, 0, 0, "缺少"},
		{"参数无效", xerr.Param("no 不合法"), "PARAM_INVALID", false, 0, 0, "不合法"},
		{"没找到", xerr.New(xerr.NotFound, "找不到这个任务"), "RESOURCE_NOT_FOUND", false, 0, 0, "找不到"},
		{"余额不足", xerr.New(xerr.InsufficientPoints, "点数不够"), "INSUFFICIENT_POINTS", false, 0, 0, "点数"},
		{"第三方故障可重试", xerr.New(xerr.ThirdPartyError, "内容检查暂时不可用"), "THIRD_PARTY_ERROR", true, 0, 0, "暂时不可用"},
		{"带包装的限频也认得", fmt.Errorf("wrap: %w", &httpapi.RateLimitError{Msg: "x", RetryAfter: 5, Check: "RATE_MINUTE"}), "RATE_LIMITED", true, 5, 0, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := MapError(c.err)
			if e.ErrorCode != c.code || e.Retryable != c.retryable || e.RetryAfterSeconds != c.retry || len(e.Gaps) != c.gaps || !strings.Contains(e.Message, c.msgHas) {
				t.Fatalf("映射不对：%+v", e)
			}
		})
	}
	// 非业务错误：不泄露内部细节，可重试
	e := MapError(errors.New("pq: connection refused 10.0.0.5:3306"))
	if e.ErrorCode != "SYSTEM_ERROR" || !e.Retryable || strings.Contains(e.Message, "10.0.0.5") || strings.Contains(e.Message, "pq") {
		t.Fatalf("内部错误不得外泄：%+v", e)
	}
	// message 不带内部码
	for _, c := range cases {
		if m := MapError(c.err).Message; strings.Contains(m, "QUOTE_CHANGED") || strings.Contains(m, "code=") {
			t.Fatalf("message 不应含内部码：%q", m)
		}
	}
}

// 端到端：后端错误经入口落成工具错误形状（HTTP 200 + is_error + result{error_code,message,retryable,gaps?}）。
func TestErrorShapeOnTheWire(t *testing.T) {
	be := newBackend()
	be.s.err = &core.GapsError{Gaps: []core.Gap{{Code: "REF_MISSING", Field: "dims.product", Message: "要一张商品图"}}}
	call, _ := chain(t, be)
	rec, e := do(t, call, signed(body("c1", "replicate_create_plan", `{"analysis_no":"VRA1"}`), 7))
	if rec.Code != 200 || !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || e.Data.Result["retryable"] != false {
		t.Fatalf("%s", rec.Body.String())
	}
	gaps, _ := e.Data.Result["gaps"].([]any)
	if len(gaps) != 1 || gaps[0].(map[string]any)["field"] != "dims.product" {
		t.Fatalf("gaps：%v", e.Data.Result)
	}
	if e.Data.Content == "" {
		t.Fatal("content 应带错误 JSON 原文，可直接回填给模型")
	}

	be = newBackend()
	be.s.err = xerr.NewReason(xerr.Conflict, "QUOTE_CHANGED", "价格变了")
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("c2", "replicate_confirm", `{"plan_no":"VRP1","plan_hash":"h","expected_points":120}`), 7))
	if !e.Data.IsError || e.Data.Result["error_code"] != "QUOTE_CHANGED" || e.Data.Replayed {
		t.Fatalf("%+v", e.Data)
	}
}

// 复刻模块没装配 / 总开关关着（Ops.Enter 回 7003）→ FEATURE_NOT_READY；读工具在总开关关着时仍可用（Enter 的 write=false 不查开关，由 Ops 保证）。
func TestFeatureNotReady(t *testing.T) {
	// 模块没装配：OpsBackend(nil)
	call, _ := chain(t, OpsBackend(nil))
	rec, e := do(t, call, signed(body("c1", "replicate_limits", `{}`), 7))
	if rec.Code != 200 || !e.Data.IsError || e.Data.Result["error_code"] != "FEATURE_NOT_READY" || e.Data.Result["retryable"] != false {
		t.Fatalf("没装配模块应 FEATURE_NOT_READY：%s", rec.Body.String())
	}
	// 总开关关着（后端回 7003）
	be := newBackend()
	be.enterErr = xerr.New(xerr.NotReady, "功能暂未开放")
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("c2", "replicate_confirm", `{"plan_no":"VRP1","plan_hash":"h","expected_points":120}`), 7))
	if !e.Data.IsError || e.Data.Result["error_code"] != "FEATURE_NOT_READY" || !be.write {
		t.Fatalf("%+v write=%v", e.Data, be.write)
	}
}

// 同 call_id 重放（Confirm 命中已有 job，JobView.Replay=true）→ 出参 replayed=true。
func TestReplayedFlag(t *testing.T) {
	be := newBackend()
	be.s.job = &core.JobView{JobID: 42, Replay: true}
	call, _ := chain(t, be)
	_, e := do(t, call, signed(body("c1", "replicate_confirm", `{"plan_no":"VRP1","plan_hash":"h","expected_points":120}`), 7))
	if e.Data.IsError || !e.Data.Replayed {
		t.Fatalf("重放应标 replayed：%+v", e.Data)
	}
}

// 定妆图回放（同一方案已出过图）标 replayed，且不是扣点工具：不要求 expected_points。
func TestPreviewReplayedFlag(t *testing.T) {
	be := newBackend()
	be.s.preview = &core.PreviewView{PlanNo: "VRP1", Replay: true}
	call, _ := chain(t, be)
	rec, e := do(t, call, signed(body("c1", "replicate_preview_frame", `{"plan_no":"VRP1"}`), 7))
	if rec.Code != 200 || e.Data.IsError || !e.Data.Replayed {
		t.Fatalf("回放应标 replayed：%s", rec.Body.String())
	}
}

// ---- 工具清单 ----

func TestTools(t *testing.T) {
	_, tools := chain(t, newBackend())
	r := httptest.NewRequest(http.MethodGet, InternalPrefix+"/agent/tools?format=openai", nil)
	svcauth.SignRequest(r, "java-1", testSecret, 0, "nonce-replicate-tools1", time.Now(), nil)
	rec := httptest.NewRecorder()
	tools(rec, r)
	var out struct {
		Code int `json:"code"`
		Data struct {
			Format string           `json:"format"`
			Tools  []map[string]any `json:"tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.Data.Format != "openai" || len(out.Data.Tools) != len(agenttool.Tools()) {
		t.Fatalf("工具清单：%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"planNo"`) || strings.Contains(rec.Body.String(), `"expectedPoints"`) {
		t.Fatal("JSON Schema 键名不得被补驼峰别名（X-Jsoncase: off）")
	}
	for _, f := range []string{"anthropic", "mcp"} {
		r = httptest.NewRequest(http.MethodGet, InternalPrefix+"/agent/tools?format="+f, nil)
		svcauth.SignRequest(r, "java-1", testSecret, 0, "nonce-replicate-tools-"+f, time.Now(), nil)
		rec = httptest.NewRecorder()
		tools(rec, r)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"replicate_confirm"`) {
			t.Fatalf("%s：%s", f, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	tools(rec, httptest.NewRequest(http.MethodGet, InternalPrefix+"/agent/tools", nil))
	if rec.Code != 401 {
		t.Fatalf("未签名应 401：%d", rec.Code)
	}
}

// 画布入口与复刻入口共用同一个校验器：同一把 key 两边都认；nonce 存储共享，一个 nonce 在任一入口只能用一次。
func TestSharedVerifierSharesNonces(t *testing.T) {
	result.Setup()
	var cfg config.CanvasInternalConf
	cfg.Keys = []config.CanvasInternalKey{{KeyId: "java-1", Secret: testSecret}}
	cfg.MaxSkewSeconds, cfg.NonceTTLSeconds = 300, 600
	v := internalauth.BuildVerifier(cfg, svcauth.NewMemoryNonces())
	a := internalauth.PreserveInternalBody(jsoncase.Middleware(CallHandler(newBackend(), v)))
	b := internalauth.PreserveInternalBody(jsoncase.Middleware(CallHandler(newBackend(), v)))
	b1 := body("c1", "replicate_limits", `{}`)
	mk := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(b1))
		svcauth.SignRequest(r, "java-1", testSecret, 7, "nonce-shared-0001", time.Now(), []byte(b1))
		return r
	}
	if rec, _ := do(t, a, mk()); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec, e := do(t, b, mk()); rec.Code != 401 || e.Reason != svcauth.ReasonReplay {
		t.Fatalf("同一个 nonce 在共享校验器下应被拒：%s", rec.Body.String())
	}
}

// 批量：只给序号没有系统注入的地址 → PARAM_INVALID，不动手；重放标记透传；batch_id 不是数字 → PARAM_INVALID。
func TestBatchDispatchGuards(t *testing.T) {
	for name, a := range map[string]string{
		"只有序号":  `{"plan_no":"VRP1","variants":[{"ref_images_from":["图1"]}],"expected_points":100}`,
		"序号地址数": `{"plan_no":"VRP1","variants":[{"ref_images_from":["图1","图2"],"ref_images":["mr://r1"]}],"expected_points":100}`,
	} {
		be := newBackend()
		call, _ := chain(t, be)
		_, e := do(t, call, signed(body("b-"+name, "replicate_confirm_batch", a), 7))
		if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || len(be.s.calls) != 0 {
			t.Fatalf("%s 应 PARAM_INVALID 且不下单：%+v", name, e.Data)
		}
	}
	be := newBackend()
	call, _ := chain(t, be)
	_, e := do(t, call, signed(body("b3", "replicate_get_batch", `{"batch_id":"abc"}`), 7))
	if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" {
		t.Fatalf("batch_id 非数字：%+v", e.Data)
	}
	// 变体为空 / 超 10 条被 Schema 挡住；缺 expected_points 在动手前被拦
	be = newBackend()
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("b4", "replicate_confirm_batch", `{"plan_no":"VRP1","variants":[{"ref_images_from":["图1"],"ref_images":["mr://r1"]}]}`), 7))
	if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || len(be.s.calls) != 0 {
		t.Fatalf("缺合计点数应拦下：%+v", e.Data)
	}
	be = newBackend()
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("b5", "replicate_quote_batch", `{"plan_no":"VRP1","variants":[]}`), 7))
	if !e.Data.IsError || len(be.s.calls) != 0 {
		t.Fatalf("空变体应被 Schema 拦下：%+v", e.Data)
	}
	// 重放
	be = newBackend()
	be.s.batchReplay = true
	call, _ = chain(t, be)
	rec, e := do(t, call, signed(body("b6", "replicate_confirm_batch", `{"plan_no":"VRP1","variants":[{"ref_images_from":["图1"],"ref_images":["mr://r1"]}],"expected_points":100}`), 7))
	if rec.Code != 200 || e.Data.IsError || !e.Data.Replayed {
		t.Fatalf("重放应带 replayed：%s", rec.Body.String())
	}
}

// §N：文案 / 导出的参数校验（平台、比例、模式不在枚举内 → PARAM_INVALID，不动手）；错误码原样透出：
// 成片未交付 JOB_NOT_DELIVERED、额度用完 COPY_QUOTA_EXHAUSTED 都不可重试；回放标记透传。
func TestPublishCopyExportDispatch(t *testing.T) {
	for name, c := range map[string][2]string{
		"平台不在枚举": {"replicate_publish_copy", `{"job_id":1,"platform":"weibo"}`},
		"比例不在枚举": {"replicate_export_ratio", `{"job_id":1,"ratio":"21:9"}`},
		"模式不在枚举": {"replicate_export_ratio", `{"job_id":1,"ratio":"1:1","mode":"stretch"}`},
		"缺比例":    {"replicate_export_ratio", `{"job_id":1}`},
	} {
		be := newBackend()
		call, _ := chain(t, be)
		_, e := do(t, call, signed(body("n-"+name, c[0], c[1]), 7))
		if !e.Data.IsError || e.Data.Result["error_code"] != "PARAM_INVALID" || len(be.s.calls) != 0 {
			t.Fatalf("%s 应 PARAM_INVALID：%+v", name, e.Data)
		}
	}
	for reason, tool := range map[string]string{"JOB_NOT_DELIVERED": "replicate_export_ratio", "COPY_QUOTA_EXHAUSTED": "replicate_publish_copy",
		"AUDIO_NOT_FAILED": "replicate_retry_audio", "AUDIO_RETRY_LIMIT": "replicate_retry_audio"} {
		be := newBackend()
		be.s.err = xerr.NewReason(xerr.Conflict, reason, "中文提示")
		call, _ := chain(t, be)
		args := `{"job_id":1,"ratio":"1:1"}`
		if tool == "replicate_publish_copy" || tool == "replicate_retry_audio" {
			args = `{"job_id":1}`
		}
		_, e := do(t, call, signed(body("n-"+reason, tool, args), 7))
		if !e.Data.IsError || e.Data.Result["error_code"] != reason || e.Data.Result["retryable"] == true {
			t.Fatalf("%s 应原样透出且不可重试：%+v", reason, e.Data)
		}
	}
	be := newBackend()
	be.s.copyReplay = true
	call, _ := chain(t, be)
	rec, _ := do(t, call, signed(body("n-replay", "replicate_publish_copy", `{"job_id":1}`), 7))
	if !strings.Contains(rec.Body.String(), `"replayed":true`) {
		t.Fatalf("回放标记应透传：%s", rec.Body.String())
	}
}

// CONTRACT §O2：body 里可选的 origin（签名覆盖）带到确认类写工具的 ctx；没带 / kind 不是 agent / 形状不对一律当没有，工具照常执行。
func TestCallOriginThreadedToConfirm(t *testing.T) {
	cases := []struct {
		name, origin, want string
	}{
		{"智能体", `,"origin":{"kind":"agent","conversation_id":"c-9","turn_id":"t-3"}`, "agent|c-9|t-3"},
		{"数字会话号", `,"origin":{"kind":"agent","conversation_id":9001}`, "agent|9001|"},
		{"没带", ``, ""},
		{"非 agent", `,"origin":{"kind":"web","conversation_id":"c-9"}`, ""},
		{"会话号非法", `,"origin":{"kind":"agent","conversation_id":"c&9"}`, ""},
		{"形状不对", `,"origin":"agent"`, ""},
	}
	for i, c := range cases {
		be := newBackend()
		call, _ := chain(t, be)
		b := fmt.Sprintf(`{"call_id":"k%d","tool":"replicate_confirm","arguments":{"plan_no":"VRP1","stage":"first","plan_hash":"h1","expected_points":120}%s}`, i, c.origin)
		rec, e := do(t, call, signed(b, 42))
		if rec.Code != 200 || e.Data.IsError || len(be.s.origins) != 1 || be.s.origins[0] != c.want {
			t.Errorf("%s：%d %s origins=%v", c.name, rec.Code, rec.Body.String(), be.s.origins)
		}
	}
	// 批量确认同样带来源
	be := newBackend()
	call, _ := chain(t, be)
	b := `{"call_id":"kb","tool":"replicate_confirm_batch","arguments":{"plan_no":"VRP1","variants":[{"ref_images_from":["图1"],"ref_images":["mr://a"]}],"expected_points":130},"origin":{"kind":"agent","conversation_id":"c-1"}}`
	if rec, e := do(t, call, signed(b, 42)); e.Data.IsError || len(be.s.origins) != 1 || be.s.origins[0] != "agent|c-1|" {
		t.Fatalf("批量确认来源：%s origins=%v", rec.Body.String(), be.s.origins)
	}
}

// 分享 / 撤回：重放标 replayed，且都不是扣点工具（不要求 expected_points）；portrait_consent 必须是布尔值。
func TestShareWithdrawFlags(t *testing.T) {
	be := newBackend()
	be.s.copyReplay = true
	call, _ := chain(t, be)
	_, e := do(t, call, signed(body("s1", "replicate_share_sample", `{"job_id":42}`), 7))
	if e.Data.IsError || !e.Data.Replayed {
		t.Fatalf("分享重放应标 replayed：%+v", e.Data)
	}
	_, e = do(t, call, signed(body("s2", "replicate_withdraw_sample", `{"submission_id":5}`), 7))
	if e.Data.IsError || !e.Data.Replayed {
		t.Fatalf("撤回重放应标 replayed：%+v", e.Data)
	}
	_, e = do(t, call, signed(body("s3", "replicate_share_sample", `{"job_id":42,"portrait_consent":"yes"}`), 7))
	if !e.Data.IsError {
		t.Fatalf("portrait_consent 不是布尔值应被拒：%+v", e.Data)
	}
}

// 连贯度：create_plan 顶层 continuity 原样带到内核；update_plan 的 continuity 是指针（不给 = 沿用，给了 = 覆盖），单独给它也算有改动。
func TestContinuityDispatch(t *testing.T) {
	be := newBackend()
	call, _ := chain(t, be)
	_, e := do(t, call, signed(body("k1", "replicate_create_plan", `{"analysis_no":"VRA1","annotations":{"dims":{"scene":{"mode":"describe","note":"海边"}}},"continuity":"smooth"}`), 7))
	if e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	if r := be.s.got["CreatePlan"].(core.PlanReq); r.Continuity != "smooth" {
		t.Fatalf("continuity 没带过去：%+v", r)
	}
	be = newBackend()
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("k2", "replicate_update_plan", `{"plan_no":"VRP1","continuity":"fast"}`), 7))
	if e.Data.IsError {
		t.Fatalf("只改连贯度应成功：%+v", e.Data)
	}
	if r := be.s.patchReq; r.Continuity == nil || *r.Continuity != "fast" {
		t.Fatalf("PatchReq.Continuity：%+v", r)
	}
	be = newBackend()
	call, _ = chain(t, be)
	_, e = do(t, call, signed(body("k3", "replicate_update_plan", `{"plan_no":"VRP1","resolution":"1080p"}`), 7))
	if e.Data.IsError || be.s.patchReq.Continuity != nil {
		t.Fatalf("没给 continuity 应为 nil（沿用）：%+v %+v", e.Data, be.s.patchReq)
	}
	_, e = do(t, call, signed(body("k4", "replicate_create_plan", `{"analysis_no":"VRA1","continuity":"slow"}`), 7))
	if !e.Data.IsError {
		t.Fatalf("非法连贯度应被 Schema 拒：%+v", e.Data)
	}
}

// 反馈工具：免费、不动账，按读入口分派（不受运行时总开关影响）；reasons / seg_index / note 原样带到内核。
func TestFeedbackDispatch(t *testing.T) {
	be := newBackend()
	call, _ := chain(t, be)
	_, e := do(t, call, signed(body("f1", "replicate_feedback", `{"job_id":42,"rating":"down","reasons":["seam_jump","other"],"seg_index":2,"note":"第二段开头跳了"}`), 7))
	if e.Data.IsError {
		t.Fatalf("%+v", e.Data)
	}
	if be.write {
		t.Fatal("反馈必须走读入口（write=false），暂停新提交时也要能收")
	}
	r := be.s.got["SubmitFeedback"].(core.FeedbackReq)
	if r.JobID != 42 || r.Rating != "down" || len(r.Reasons) != 2 || r.Reasons[0] != "seam_jump" || r.SegIndex != 2 || r.Note != "第二段开头跳了" {
		t.Fatalf("入参：%+v", r)
	}
	for name, a := range map[string]string{
		"评价非法": `{"job_id":42,"rating":"meh"}`,
		"原因非法": `{"job_id":42,"rating":"down","reasons":["bad"]}`,
		"段号为零": `{"job_id":42,"rating":"down","seg_index":0}`,
	} {
		be = newBackend()
		call, _ = chain(t, be)
		_, e = do(t, call, signed(body("f-"+name, "replicate_feedback", a), 7))
		if !e.Data.IsError || len(be.s.calls) != 0 {
			t.Fatalf("%s 应被 Schema 拒：%+v", name, e.Data)
		}
	}
}
