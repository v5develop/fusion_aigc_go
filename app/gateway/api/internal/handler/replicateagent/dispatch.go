package replicateagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"aigc-local-life/app/replicate/agenttool"
	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/httpapi"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// Session 一次工具调用要用到的复刻操作（httpapi.Scope 的子集）。拆成接口只为单测可以换替身；生产由 OpsBackend 适配 httpapi.Ops。
type Session interface {
	Limits() httpapi.LimitsView
	CreateAnalysis(ctx context.Context, sourceURL string) (*core.AnalysisView, error)
	GetAnalysis(ctx context.Context, no string) (*core.AnalysisView, error)
	CreatePlan(ctx context.Context, req core.PlanReq) (*core.PlanView, error)
	QuotePlan(ctx context.Context, no string) (*core.PlanView, error)
	Confirm(ctx context.Context, a httpapi.ConfirmArgs) (*core.JobView, error)
	JobView(ctx context.Context, id int64) (*core.JobView, error)
	ListJobs(ctx context.Context, page, size int) (*core.JobList, error)
	Continue(ctx context.Context, a httpapi.ContinueArgs) (*core.JobView, error)
	Stop(ctx context.Context, id int64) (*core.JobView, error)
	CancelRest(ctx context.Context, id int64) (*core.JobView, error)
	Models(ctx context.Context, req core.ModelsReq) (*core.ModelList, error)
	Redo(ctx context.Context, a httpapi.RedoArgs) (*core.JobView, error)
	ListSamples(ctx context.Context, industry string) (*core.SampleList, error)
	TrySample(ctx context.Context, id int64) (*core.SampleTry, error)
	GetPlan(ctx context.Context, no string) (*core.PlanView, error)
	PreviewFrame(ctx context.Context, no string) (*core.PreviewView, error)
	PatchAnnotations(ctx context.Context, req core.PatchReq, hasAnnotations bool) (*core.PlanView, error)
	QuoteBatch(ctx context.Context, planNo string, variants []httpapi.BatchVariantArgs) (*core.BatchQuoteView, error)
	ConfirmBatch(ctx context.Context, a httpapi.BatchConfirmArgs) (*core.BatchView, error)
	GetBatch(ctx context.Context, id int64) (*core.BatchView, error)
	PublishCopy(ctx context.Context, jobID int64, platform string) (*core.PublishCopyView, error)
	ExportRatio(ctx context.Context, jobID int64, ratio, mode string) (*core.ExportView, error)
	ListVoices(ctx context.Context, gender, style string) (*core.VoiceList, error)
	RetryAudio(ctx context.Context, jobID int64, callID string) (*core.AudioRetryView, error)
	Trending(ctx context.Context, industry string) (*core.TrendingView, error)
	ShareSample(ctx context.Context, jobID int64, portraitConsent bool) (*core.ShareView, error)
	WithdrawSample(ctx context.Context, submissionID int64) (*core.WithdrawView, error)
}

// Backend 取调用上下文（模块在 / 已登录 / 写工具另查总开关）。
type Backend interface {
	Enter(ctx context.Context, uid int64, write bool) (Session, error)
}

type opsBackend struct{ o *httpapi.Ops }

// OpsBackend 生产后端：o 为 nil（复刻模块没装配）时 Enter 回「功能暂未开放」。
func OpsBackend(o *httpapi.Ops) Backend { return opsBackend{o} }

func (b opsBackend) Enter(ctx context.Context, uid int64, write bool) (Session, error) {
	sc, err := b.o.EnterAs(ctx, uid, write, core.ChannelAgent)
	if err != nil {
		return nil, err
	}
	return sc, nil
}

// tool 一个工具的执行函数：args 已按 Schema 校验；key = call_id（写工具的幂等键）。
type tool struct {
	write bool
	run   func(ctx context.Context, s Session, a args, key string) (any, error)
}

// Execute 执行一次工具调用：查工具 → 校验入参 → 扣点工具先查 expected_points → Enter → 分派。所有失败都折成工具错误（不返回 error）。
func Execute(ctx context.Context, be Backend, uid int64, c agenttool.Call) *agenttool.Result {
	t, ok := agenttool.Find(c.Tool)
	h, hok := handlers[c.Tool]
	if !ok || !hok {
		logx.WithContext(ctx).Infof("replicate agent: 工具调用被拒 判据=UNKNOWN_TOOL tool=%s call=%s user=%d", c.Tool, c.CallID, uid)
		return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrUnknownTool, Message: "没有这个工具：" + c.Tool})
	}
	m, err := agenttool.ParseArguments(t, c.Arguments)
	if err != nil {
		logx.WithContext(ctx).Infof("replicate agent: 工具调用被拒 判据=ARGS_INVALID tool=%s call=%s user=%d: %v", c.Tool, c.CallID, uid, err)
		return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrParam, Message: "参数不对：" + err.Error()})
	}
	a := args(m)
	// 扣点工具：expected_points 必填且 >0，在查重放 / 限频 / 动钱之前拦下（CONTRACT §A；不走 core「0 = 不比对」）
	if t.Charges {
		if p, ok := a.int("expected_points"); !ok || p <= 0 {
			logx.WithContext(ctx).Infof("replicate agent: 工具调用被拒 判据=EXPECTED_POINTS_REQUIRED tool=%s call=%s user=%d", c.Tool, c.CallID, uid)
			return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrParam, Message: "缺少本次扣点数（expected_points），请先让用户确认金额后再执行"})
		}
	}
	s, err := be.Enter(ctx, uid, h.write)
	if err != nil {
		return failure(ctx, c, uid, err)
	}
	v, err := h.run(ctx, s, a, c.CallID)
	if err != nil {
		return failure(ctx, c, uid, err)
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		logx.WithContext(ctx).Errorf("replicate agent: 工具结果序列化失败 判据=MARSHAL_FAILED tool=%s call=%s: %v", c.Tool, c.CallID, merr)
		return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrServer, Message: "服务繁忙，请稍后重试", Retryable: true})
	}
	res := &agenttool.Result{Data: b}
	if jv, ok := v.(*core.JobView); ok && jv != nil {
		res.Replayed = jv.Replay // 同 call_id 重放 / 方案已确认：返回的是已有任务
	}
	if bv, ok := v.(*core.BatchView); ok && bv != nil {
		res.Replayed = bv.Replay // 同 call_id 重放：返回的是已有批次，没有重复扣点
	}
	if cv, ok := v.(*core.PublishCopyView); ok && cv != nil {
		res.Replayed = cv.Replay // 同成片同平台已写过：回放，没占额度
	}
	if ev, ok := v.(*core.ExportView); ok && ev != nil {
		res.Replayed = ev.Replay || ev.Original
	}
	if sv, ok := v.(*core.ShareView); ok && sv != nil {
		res.Replayed = sv.Replay // 这条成片早就投过：返回的是已有投稿
	}
	if wv, ok := v.(*core.WithdrawView); ok && wv != nil {
		res.Replayed = wv.Replay
	}
	if av, ok := v.(*core.AudioRetryView); ok && av != nil {
		res.Replayed = av.Replay // 同 call_id 重放：返回当时的结果，没有重做也没有计次
	}
	if pv, ok := v.(*core.PreviewView); ok && pv != nil {
		res.Replayed = pv.Replay // 同一方案已出过图：回放，没占额度
	}
	return res
}

func failure(ctx context.Context, c agenttool.Call, uid int64, err error) *agenttool.Result {
	e := MapError(err)
	if e.ErrorCode == agenttool.ErrServer {
		logx.WithContext(ctx).Errorf("replicate agent: 工具执行失败 判据=INTERNAL_ERROR tool=%s call=%s user=%d: %v", c.Tool, c.CallID, uid, err)
	} else {
		logx.WithContext(ctx).Infof("replicate agent: 工具执行被拒 判据=%s tool=%s call=%s user=%d: %s", e.ErrorCode, c.Tool, c.CallID, uid, e.Message)
	}
	return agenttool.ErrorResult(e)
}

// retryableReasons 同样的调用稍后可能成功的 reason；其余（QUOTE_CHANGED / PLAN_STALE / PARAM_INVALID …）要换参数 / 重新确认才行。
var retryableReasons = map[string]bool{
	"MODERATION_PENDING":     true, // 内容检查还没出结论，几秒后再试
	"MODERATION_UNAVAILABLE": true,
	"IN_PROGRESS":            true,
	"THIRD_PARTY_ERROR":      true,
	agenttool.ErrServer:      true,
}

// MapError 把 Ops / core 的错误映射成工具错误 {error_code, message, retryable, gaps?}（CONTRACT §A）。
// error_code 取 Go reason 原样（QUOTE_CHANGED / PLAN_STALE / PLAN_EXPIRED / NOT_REDOABLE / MODERATION_REJECTED / FEATURE_NOT_READY …），
// 只做两处归并：PARAM_MISSING → PARAM_INVALID（工具侧不区分缺失与无效）、非业务错误 → SYSTEM_ERROR（文案不带内部细节，可重试）。
// message 是 core 给用户看的中文，不含内部码与供应商原文。
func MapError(err error) agenttool.ErrorData {
	var rl *httpapi.RateLimitError
	if errors.As(err, &rl) {
		// 每日额度用完不可重试（要等明天）；每分钟 / 全局在途稍后可重试
		return agenttool.ErrorData{ErrorCode: agenttool.ErrRateLimited, Message: rl.Msg, Retryable: rl.Check != "RATE_DAY", RetryAfterSeconds: rl.RetryAfter}
	}
	var ge *core.GapsError
	if errors.As(err, &ge) {
		gaps := make([]agenttool.Gap, len(ge.Gaps))
		for i, g := range ge.Gaps {
			gaps[i] = agenttool.Gap{Code: g.Code, Field: g.Field, Message: g.Message}
		}
		return agenttool.ErrorData{ErrorCode: agenttool.ErrParam, Message: ge.Error(), Gaps: gaps}
	}
	if _, msg, ok := xerr.Parse(err); ok {
		reason := xerr.ReasonOf(err)
		switch reason {
		case "PARAM_MISSING":
			reason = agenttool.ErrParam
		case "":
			reason = agenttool.ErrServer
		}
		return agenttool.ErrorData{ErrorCode: reason, Message: msg, Retryable: retryableReasons[reason]}
	}
	return agenttool.ErrorData{ErrorCode: agenttool.ErrServer, Message: "服务繁忙，请稍后重试", Retryable: true}
}

// ---------------------------------------------------------------- 入参取值

type args map[string]any

func (a args) str(k string) string {
	s, _ := a[k].(string)
	return strings.TrimSpace(s)
}

func (a args) has(k string) bool { _, ok := a[k]; return ok }

// int 取整数（Schema 已校验是整数；JSON 数字解出来是 float64）。
func (a args) int(k string) (int64, bool) {
	f, ok := a[k].(float64)
	return int64(f), ok
}

func (a args) strs(k string) []string {
	xs, _ := a[k].([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func paramErr(msg string) error { return xerr.Param(msg) }

// annotations 把校验过的 annotations 对象还原成 core.Annotations（键名与 core 的 json 标签一致，直接走一次 JSON）。
func (a args) annotations() (core.Annotations, bool, error) {
	raw, ok := a["annotations"]
	if !ok {
		return core.Annotations{}, false, nil
	}
	b, _ := json.Marshal(raw)
	var ann core.Annotations
	if err := json.Unmarshal(b, &ann); err != nil {
		return core.Annotations{}, true, paramErr("annotations 格式不对")
	}
	return ann, true, nil
}

// refImages 取参考图地址：ref_images 是系统按 ref_images_from 注入的；只给了序号却没有地址 = 没能对应到用户上传的图，如实报错而不是静默丢图。
// 返回 (地址, 是否出现了这个字段)。
func (a args) refImages() ([]string, bool, error) {
	from, hasFrom := a["ref_images_from"]
	imgs, hasImgs := a["ref_images"]
	if hasFrom && !hasImgs {
		if xs, _ := from.([]any); len(xs) > 0 {
			return nil, true, paramErr("参考图没能对应到你上传的图片，请重新上传图片后再试")
		}
	}
	if hasFrom && hasImgs {
		f, _ := from.([]any)
		i, _ := imgs.([]any)
		if len(f) != len(i) {
			return nil, true, paramErr("参考图没能对应到你上传的图片，请重新上传图片后再试")
		}
	}
	if !hasImgs && !hasFrom {
		return nil, false, nil
	}
	out := []string{}
	xs, _ := imgs.([]any)
	for _, x := range xs {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out, true, nil
}

// batchVariants 取批量的变体：variants[].ref_images 是系统按 ref_images_from 注入的真实地址；
// 只给了序号没有地址 = 没能对应到用户上传的图，如实报错而不是静默丢图。
func (a args) batchVariants() ([]httpapi.BatchVariantArgs, error) {
	xs, _ := a["variants"].([]any)
	out := make([]httpapi.BatchVariantArgs, 0, len(xs))
	for _, x := range xs {
		m, _ := x.(map[string]any)
		from, _ := m["ref_images_from"].([]any)
		imgs, hasImgs := m["ref_images"].([]any)
		if (len(from) > 0 && !hasImgs) || (len(from) > 0 && len(from) != len(imgs)) {
			return nil, paramErr("参考图没能对应到你上传的图片，请重新上传图片后再试")
		}
		v := httpapi.BatchVariantArgs{}
		for _, i := range imgs {
			if s, ok := i.(string); ok && strings.TrimSpace(s) != "" {
				v.RefImages = append(v.RefImages, strings.TrimSpace(s))
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// mergeAnnotations 把本次给的标注按维度合并进当前方案的标注：dims 逐个覆盖，audio 给了才换，texts 出现（含空数组）就整体替换。
func mergeAnnotations(cur *core.Annotations, patch core.Annotations, hasAudio, hasTexts, hasVoice bool) core.Annotations {
	out := core.Annotations{Dims: map[string]core.DimEdit{}}
	if cur != nil {
		out.Audio, out.Texts, out.Voice = cur.Audio, append([]core.TextOverlay(nil), cur.Texts...), cur.Voice
		for k, v := range cur.Dims {
			out.Dims[k] = v
		}
	}
	for k, v := range patch.Dims {
		out.Dims[k] = v
	}
	if hasAudio {
		out.Audio = patch.Audio
	}
	if hasTexts {
		out.Texts = patch.Texts
	}
	if hasVoice {
		out.Voice = patch.Voice // 换声音设置整体替换
	} else if hasAudio && out.Audio != core.AudioRevoice {
		out.Voice = nil // 声音改成别的方式，旧的音色设置不再有效
	}
	return out
}

// ---------------------------------------------------------------- 工具表

var handlers = map[string]tool{
	agenttool.ToolLimits: {run: func(_ context.Context, s Session, _ args, _ string) (any, error) {
		return s.Limits(), nil
	}},
	agenttool.ToolAnalyze: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		src := a.str("source")
		if src == "" {
			return nil, paramErr("没有拿到要分析的视频，请让用户先上传视频再试")
		}
		return s.CreateAnalysis(ctx, src)
	}},
	agenttool.ToolGetAnalysis: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.GetAnalysis(ctx, a.str("analysis_no"))
	}},
	agenttool.ToolListModels: {run: func(ctx context.Context, s Session, _ args, _ string) (any, error) {
		return s.Models(ctx, core.ModelsReq{})
	}},
	agenttool.ToolListVoices: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.ListVoices(ctx, a.str("gender"), a.str("style"))
	}},
	agenttool.ToolListSamples: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.ListSamples(ctx, a.str("industry"))
	}},
	agenttool.ToolTrySample: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("sample_id")
		return s.TrySample(ctx, id)
	}},
	agenttool.ToolCreatePlan: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		ann, _, err := a.annotations()
		if err != nil {
			return nil, err
		}
		refs, _, err := a.refImages()
		if err != nil {
			return nil, err
		}
		sid, _ := a.int("sample_id")
		return s.CreatePlan(ctx, core.PlanReq{AnalysisNo: a.str("analysis_no"), SampleID: sid, Annotations: ann, RefImages: refs,
			ModelGroup: a.str("model_group"), Resolution: a.str("resolution"), AspectRatio: a.str("aspect_ratio")})
	}},
	agenttool.ToolUpdatePlan: {write: true, run: updatePlan},
	agenttool.ToolGetPlan: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.GetPlan(ctx, a.str("plan_no"))
	}},
	// 定妆图：免费、不是扣点工具（无 expected_points / 同意卡）；按写入口处理，让运行时总开关能一并拦住
	agenttool.ToolPreviewFrame: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.PreviewFrame(ctx, a.str("plan_no"))
	}},
	agenttool.ToolQuote: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.QuotePlan(ctx, a.str("plan_no"))
	}},
	agenttool.ToolConfirm: {write: true, run: func(ctx context.Context, s Session, a args, key string) (any, error) {
		pts, _ := a.int("expected_points")
		return s.Confirm(ctx, httpapi.ConfirmArgs{PlanNo: a.str("plan_no"), PlanHash: a.str("plan_hash"), Stage: a.str("stage"),
			ExpectedPoints: &pts, IdempotencyKey: key})
	}},
	agenttool.ToolQuoteBatch: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		vs, err := a.batchVariants()
		if err != nil {
			return nil, err
		}
		return s.QuoteBatch(ctx, a.str("plan_no"), vs)
	}},
	agenttool.ToolConfirmBatch: {write: true, run: func(ctx context.Context, s Session, a args, key string) (any, error) {
		vs, err := a.batchVariants()
		if err != nil {
			return nil, err
		}
		pts, _ := a.int("expected_points")
		return s.ConfirmBatch(ctx, httpapi.BatchConfirmArgs{PlanNo: a.str("plan_no"), Variants: vs, ExpectedPoints: &pts, IdempotencyKey: key})
	}},
	agenttool.ToolGetBatch: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, err := core.ParseBatchID(a.str("batch_id"))
		if err != nil {
			return nil, err
		}
		return s.GetBatch(ctx, id)
	}},
	agenttool.ToolPublishCopy: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("job_id")
		return s.PublishCopy(ctx, id, a.str("platform"))
	}},
	agenttool.ToolExportRatio: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("job_id")
		return s.ExportRatio(ctx, id, a.str("ratio"), a.str("mode"))
	}},
	agenttool.ToolRetryAudio: {write: true, run: func(ctx context.Context, s Session, a args, key string) (any, error) {
		id, _ := a.int("job_id")
		return s.RetryAudio(ctx, id, key)
	}},
	agenttool.ToolTrending: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		return s.Trending(ctx, a.str("industry"))
	}},
	agenttool.ToolShareSample: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("job_id")
		consent, _ := a["portrait_consent"].(bool)
		return s.ShareSample(ctx, id, consent)
	}},
	agenttool.ToolWithdrawSample: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("submission_id")
		return s.WithdrawSample(ctx, id)
	}},
	agenttool.ToolGetJob: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("job_id")
		return s.JobView(ctx, id)
	}},
	agenttool.ToolListJobs: {run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		page, _ := a.int("page")
		return s.ListJobs(ctx, int(page), 0)
	}},
	agenttool.ToolContinue: {write: true, run: func(ctx context.Context, s Session, a args, key string) (any, error) {
		id, _ := a.int("job_id")
		pts, _ := a.int("expected_points")
		return s.Continue(ctx, httpapi.ContinueArgs{JobID: id, PlanHash: a.str("plan_hash"), ExpectedPoints: &pts, IdempotencyKey: key})
	}},
	agenttool.ToolRedoSegment: {write: true, run: func(ctx context.Context, s Session, a args, key string) (any, error) {
		id, _ := a.int("job_id")
		seg, _ := a.int("seg_index")
		pts, _ := a.int("expected_points")
		return s.Redo(ctx, httpapi.RedoArgs{JobID: id, SegIndex: seg, Note: a.str("note"), ExpectedPoints: &pts, IdempotencyKey: key})
	}},
	agenttool.ToolStop: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("job_id")
		return s.Stop(ctx, id)
	}},
	agenttool.ToolCancelRest: {write: true, run: func(ctx context.Context, s Session, a args, _ string) (any, error) {
		id, _ := a.int("job_id")
		return s.CancelRest(ctx, id)
	}},
}

// updatePlan 改方案：annotations 按维度合并进当前方案（core.PatchAnnotations 本身是整体替换，所以先取当前方案的标注再叠）；
// 其余字段给了才改（nil = 沿用）。什么都没给 = 无事可做。
func updatePlan(ctx context.Context, s Session, a args, _ string) (any, error) {
	no := a.str("plan_no")
	ann, hasAnn, err := a.annotations()
	if err != nil {
		return nil, err
	}
	refs, hasRefs, err := a.refImages()
	if err != nil {
		return nil, err
	}
	req := core.PatchReq{PlanNo: no}
	if hasRefs {
		req.RefImages = &refs
	}
	for k, dst := range map[string]**string{"model_group": &req.ModelGroup, "resolution": &req.Resolution, "aspect_ratio": &req.AspectRatio} {
		if a.has(k) {
			v := a.str(k)
			*dst = &v
		}
	}
	if !hasAnn && !hasRefs && req.ModelGroup == nil && req.Resolution == nil && req.AspectRatio == nil {
		return nil, paramErr("没有要修改的内容：请给出要改的维度、参考图、模型、分辨率或画幅")
	}
	cur, err := s.GetPlan(ctx, no)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这张方案")
	}
	raw, _ := a["annotations"].(map[string]any)
	_, hasAudio := raw["audio"]
	_, hasTexts := raw["texts"]
	_, hasVoice := raw["voice"]
	req.Annotations = mergeAnnotations(cur.Annotations, ann, hasAudio, hasTexts, hasVoice)
	return s.PatchAnnotations(ctx, req, true)
}
