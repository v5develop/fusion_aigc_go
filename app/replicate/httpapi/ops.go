package httpapi

import (
	"context"
	"strings"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/xerr"
)

// Ops 与传输无关的复刻操作层：REST（routes.go / library_routes.go）与内部智能体入口（app/gateway/api/internal/handler/replicateagent）
// 都只调它，两条路径共用同一份「取内核 → 总开关 → 参数口径 → 限频 take/refund → requirePoints / stage 校验 → 重放短路 → 写后任务中心同步」。
// 方法都是强类型的，不碰 *http.Request：传输层只负责把入参解析成类型值、把错误翻译成各自的响应形状。
//
// 用法：先 Enter 拿到本次调用的 Scope（查 Module / 登录用户 / 运行时总开关），再调 Scope 上的方法。
// Enter 单独成一步是为了保持 REST 原有的判定顺序（未开启 / 未登录 / 总开关先于请求体解析）。
type Ops struct{ m *Module }

// Ops 取操作层；Module 为 nil（没装配）返回 nil，nil *Ops 上的 Enter 回 7003「功能暂未开放」。
func (m *Module) Ops() *Ops {
	if m == nil || m.rt == nil {
		return nil
	}
	return &Ops{m: m}
}

// Scope 一次调用的上下文：已通过「模块在 / 用户已登录 / 写入口总开关开着」。
type Scope struct {
	o       *Ops
	s       *core.Service
	uid     int64
	write   bool
	enabled bool
	channel string // 调用渠道（core.ChannelWeb / core.ChannelAgent），漏斗埋点按它切分
}

// Enter REST 入口：渠道记为 web。
func (o *Ops) Enter(ctx context.Context, uid int64, write bool) (*Scope, error) {
	return o.EnterAs(ctx, uid, write, core.ChannelWeb)
}

// EnterAs 取内核与登录用户，写入口另查运行时总开关（关着回 7003；读入口照常，已有任务能看）。
// channel 是调用渠道（智能体内部入口传 core.ChannelAgent），只用于漏斗埋点，不改变任何业务口径。
func (o *Ops) EnterAs(ctx context.Context, uid int64, write bool, channel string) (*Scope, error) {
	if o == nil || o.m == nil || o.m.rt == nil {
		return nil, errNotReady()
	}
	if uid <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	s, enabled := o.m.rt.Service(ctx)
	if s == nil || (write && !enabled) {
		return nil, errNotReady()
	}
	return &Scope{o: o, s: s, uid: uid, write: write, enabled: enabled, channel: channel}, nil
}

// ctx 把调用渠道带进内核（漏斗埋点读取）；写方法调内核前都过一遍。
func (sc *Scope) ctx(ctx context.Context) context.Context { return core.WithChannel(ctx, sc.channel) }

// take 限频（ratelimit.go）：命中返回 *RateLimitError。
func (sc *Scope) take(ctx context.Context, action string) (func(), error) {
	if sc.o.m.limiter == nil {
		return func() {}, nil
	}
	return sc.o.m.limiter.take(ctx, action, sc.uid, sc.o.m.rt.Rate())
}

// kick 写成功后让任务中心尽快看到新状态（轮询器另有周期同步兜底）。
func (sc *Scope) kick() {
	if sc.write && sc.o.m.taskSync != nil {
		sc.o.m.taskSync.Kick()
	}
}

// done 写方法的收尾：成功才 Kick。
func (sc *Scope) done(err error) error {
	if err == nil {
		sc.kick()
	}
	return err
}

// 路径参数口径（REST 的 :no / :id / :k 与智能体工具入参共用，报错文案一致）。

func checkStr(v, name string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return "", xerr.Param(name + " 不合法")
	}
	return v, nil
}

func checkID(n int64, name string) error {
	if n <= 0 {
		return xerr.Param(name + " 不合法")
	}
	return nil
}

// requirePoints 确认 / 继续 / 重做都要预留点数：必须带页面展示的本次点数（与后端重算不一致 → 409 QUOTE_CHANGED；
// 确认时方案总价超过 replicate.confirm_threshold_points 时缺失或不一致同样 409 QUOTE_CHANGED，见 core.gateConfirmThreshold）。
// 缺失或 ≤0 → 1002 PARAM_MISSING，在查重放、限频、动钱之前拦下。
func requirePoints(p *int64, what string) (int64, error) {
	if p == nil || *p <= 0 {
		return 0, xerr.New(xerr.ParamMissing, "缺少"+what+"点数（expectedPoints），请刷新页面后重试")
	}
	return *p, nil
}

// LimitsView limits 加上运行时总开关：前端据此在页头说清「暂停新提交」，而不是等用户点了才报 7003。
type LimitsView struct {
	core.LimitsView
	// Enabled 当前是否接新提交（afv_system_config.replicate.enabled，库里没有时取 Replicate.Enabled）
	Enabled bool `json:"enabled"`
}

// Limits 当前限制与总开关。
func (sc *Scope) Limits() LimitsView {
	return LimitsView{LimitsView: sc.s.Limits(), Enabled: sc.enabled}
}

// CreateAnalysis 上传的视频做分析（限频 analysis；失败 / 命中同一条视频的复用分析不计次）。
func (sc *Scope) CreateAnalysis(ctx context.Context, sourceURL string) (*core.AnalysisView, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return nil, xerr.New(xerr.ParamMissing, "请先上传要做同款的视频")
	}
	if len(strings.TrimSpace(sourceURL)) > 1024 {
		return nil, xerr.Param("视频地址太长了，请重新上传这条视频")
	}
	refund, err := sc.take(ctx, ActionAnalysis)
	if err != nil {
		return nil, err
	}
	v, err := sc.s.CreateAnalysis(sc.ctx(ctx), core.CreateAnalysisReq{UserID: sc.uid, SourceURL: sourceURL})
	if err != nil || (v != nil && v.Cached) {
		refund() // 失败不计次；同一条视频复用已有分析（单飞）也不计次（EXPERIENCE 计次口径 F-22）
	}
	return v, sc.done(err)
}

// GetAnalysis 取分析。
func (sc *Scope) GetAnalysis(ctx context.Context, no string) (*core.AnalysisView, error) {
	no, err := checkStr(no, "no")
	if err != nil {
		return nil, err
	}
	return sc.s.GetAnalysis(ctx, sc.uid, no)
}

// CreatePlan 出方案（限频 plan；缺项 / 失败不占次数）。req.UserID 以登录用户为准。
func (sc *Scope) CreatePlan(ctx context.Context, req core.PlanReq) (*core.PlanView, error) {
	if strings.TrimSpace(req.AnalysisNo) == "" && req.SampleID <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少分析编号，请重新上传视频")
	}
	refund, err := sc.take(ctx, ActionPlan)
	if err != nil {
		return nil, err
	}
	req.UserID = sc.uid
	v, err := sc.s.CreatePlan(sc.ctx(ctx), req)
	if err != nil {
		refund() // 缺项 / 失败不占方案次数
	}
	return v, sc.done(err)
}

// QuotePlan 重新报价。
func (sc *Scope) QuotePlan(ctx context.Context, no string) (*core.PlanView, error) {
	no, err := checkStr(no, "no")
	if err != nil {
		return nil, err
	}
	v, err := sc.s.QuotePlan(ctx, sc.uid, no)
	return v, sc.done(err)
}

// ConfirmArgs 确认方案的入参。
type ConfirmArgs struct {
	PlanNo, PlanHash string
	Stage            string // first / all；空 = 默认
	ExpectedPoints   *int64
	IdempotencyKey   string
}

// Confirm 确认方案（扣点）：expectedPoints 必填 >0 → stage 校验 → 重放 / 已确认直接返回已有 job（不限频不计次）→ 限频 confirm → 内核。
func (sc *Scope) Confirm(ctx context.Context, a ConfirmArgs) (*core.JobView, error) {
	no, err := checkStr(a.PlanNo, "no")
	if err != nil {
		return nil, err
	}
	pts, err := requirePoints(a.ExpectedPoints, "确认")
	if err != nil {
		return nil, err
	}
	if a.Stage != "" && a.Stage != "first" && a.Stage != "all" {
		return nil, xerr.Param("stage 只能是 first 或 all")
	}
	// 重放 / 方案已确认：直接返回已有 job，不走限频、不计次
	if v, err := sc.s.ExistingConfirm(ctx, sc.uid, no, a.IdempotencyKey); err != nil || v != nil {
		return v, sc.done(err)
	}
	refund, err := sc.take(ctx, ActionConfirm)
	if err != nil {
		return nil, err
	}
	v, err := sc.s.Confirm(sc.ctx(ctx), core.ConfirmReq{UserID: sc.uid, PlanNo: no, PlanHash: a.PlanHash, Stage: a.Stage,
		IdempotencyKey: a.IdempotencyKey, ExpectedPoints: pts})
	if err != nil || (v != nil && v.Replay) {
		refund() // 没确认成功（审核没过 / 价变 / 余额不足…）或只是重放已有 job：不占次数
	}
	return v, sc.done(err)
}

// JobView 取任务。
func (sc *Scope) JobView(ctx context.Context, id int64) (*core.JobView, error) {
	if err := checkID(id, "id"); err != nil {
		return nil, err
	}
	return sc.s.JobView(ctx, sc.uid, id)
}

// ListJobs 本人历史（创建倒序；page 从 1 起默认 1，size 默认 20；0 = 默认，负数不合法）。
func (sc *Scope) ListJobs(ctx context.Context, page, size int) (*core.JobList, error) {
	page, size, err := pageOf(page, size)
	if err != nil {
		return nil, err
	}
	return sc.s.ListJobs(ctx, sc.uid, page, size)
}

func pageOf(page, size int) (int, int, error) {
	if page < 0 {
		return 0, 0, xerr.Param("page 不合法")
	}
	if size < 0 {
		return 0, 0, xerr.Param("size 不合法")
	}
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 20
	}
	return page, size, nil
}

// ContinueArgs 继续其余段的入参。
type ContinueArgs struct {
	JobID          int64
	PlanHash       string
	ExpectedPoints *int64
	IdempotencyKey string
}

// Continue 继续做其余段（扣点）。
func (sc *Scope) Continue(ctx context.Context, a ContinueArgs) (*core.JobView, error) {
	if err := checkID(a.JobID, "id"); err != nil {
		return nil, err
	}
	pts, err := requirePoints(a.ExpectedPoints, "继续")
	if err != nil {
		return nil, err
	}
	v, err := sc.s.Continue(sc.ctx(ctx), core.ContinueReq{UserID: sc.uid, JobID: a.JobID, PlanHash: a.PlanHash,
		IdempotencyKey: a.IdempotencyKey, ExpectedPoints: pts})
	return v, sc.done(err)
}

// Stop 停止任务（退点）。
func (sc *Scope) Stop(ctx context.Context, id int64) (*core.JobView, error) {
	if err := checkID(id, "id"); err != nil {
		return nil, err
	}
	v, err := sc.s.Stop(sc.ctx(ctx), sc.uid, id)
	return v, sc.done(err)
}

// CancelRest 不做剩下的段（只取消没开始的段，在途段做完；幂等）。
func (sc *Scope) CancelRest(ctx context.Context, id int64) (*core.JobView, error) {
	if err := checkID(id, "id"); err != nil {
		return nil, err
	}
	v, err := sc.s.CancelRest(sc.ctx(ctx), sc.uid, id)
	return v, sc.done(err)
}

// Models 可选出片模型。req.UserID 以登录用户为准。
func (sc *Scope) Models(ctx context.Context, req core.ModelsReq) (*core.ModelList, error) {
	req.UserID = sc.uid
	return sc.s.ListModels(ctx, req)
}

// ListVoices 换声音的音色列表（CONTRACT §P，只读）。gender: female / male；style: 风格关键词。
func (sc *Scope) ListVoices(ctx context.Context, gender, style string) (*core.VoiceList, error) {
	return sc.s.ListVoices(ctx, core.VoiceListReq{Gender: gender, Style: style})
}

// Trending 灵感推荐（CONTRACT §S，只读）：本周同行业复刻最多的改法。industry 空 = 取用户在广场设置的行业偏好，再缺省全站。
func (sc *Scope) Trending(ctx context.Context, industry string) (*core.TrendingView, error) {
	return sc.s.Trending(ctx, sc.uid, industry)
}

// RedoArgs 单段重做的入参。
type RedoArgs struct {
	JobID          int64
	SegIndex       int64
	Note           string // 单段精修备注（可选，≤200 字，经审核，只写进这一段这次重做的提示词）
	ExpectedPoints *int64
	IdempotencyKey string
}

// Redo 单段重做（扣点，报价只算这一段）。
func (sc *Scope) Redo(ctx context.Context, a RedoArgs) (*core.JobView, error) {
	if err := checkID(a.JobID, "id"); err != nil {
		return nil, err
	}
	if err := checkID(a.SegIndex, "k"); err != nil {
		return nil, err
	}
	pts, err := requirePoints(a.ExpectedPoints, "重做")
	if err != nil {
		return nil, err
	}
	v, err := sc.s.Redo(sc.ctx(ctx), core.RedoReq{UserID: sc.uid, JobID: a.JobID, SegIndex: int(a.SegIndex), IdempotencyKey: a.IdempotencyKey,
		ExpectedPoints: pts, Note: a.Note})
	return v, sc.done(err)
}

// SetAutoContinue 「以后直接全部做」。
func (sc *Scope) SetAutoContinue(ctx context.Context, on *bool) (map[string]any, error) {
	if on == nil {
		return nil, xerr.New(xerr.ParamMissing, "缺少 autoContinue")
	}
	if err := sc.s.SetAutoContinue(ctx, sc.uid, *on); err != nil {
		return nil, err
	}
	sc.kick()
	return map[string]any{"autoContinue": *on}, nil
}

// SaveSegment 把已做好的这一段保存到素材库（幂等；入库异步）。
func (sc *Scope) SaveSegment(ctx context.Context, jobID, k int64) (*core.SaveSegmentView, error) {
	if err := checkID(jobID, "id"); err != nil {
		return nil, err
	}
	if err := checkID(k, "k"); err != nil {
		return nil, err
	}
	v, err := sc.s.SaveSegment(sc.ctx(ctx), sc.uid, jobID, int(k))
	return v, sc.done(err)
}

// ListSamples 已启用且已有分析的示例（只读，不计限频）。
func (sc *Scope) ListSamples(ctx context.Context, industry string) (*core.SampleList, error) {
	industry = strings.TrimSpace(industry)
	if len(industry) > 64 {
		return nil, xerr.Param("industry 不合法")
	}
	return sc.s.ListSamples(ctx, industry)
}

// TrySample 共享分析 + 示例成片，零成本（不建 job、不预留、不计限频）。
func (sc *Scope) TrySample(ctx context.Context, id int64) (*core.SampleTry, error) {
	if err := checkID(id, "id"); err != nil {
		return nil, err
	}
	return sc.s.TrySample(ctx, id)
}

// GetPlan 取方案。
func (sc *Scope) GetPlan(ctx context.Context, no string) (*core.PlanView, error) {
	no, err := checkStr(no, "no")
	if err != nil {
		return nil, err
	}
	return sc.s.GetPlan(ctx, sc.uid, no)
}

// PreviewFrame 方案的定妆图（免费，每人每天 N 次，额度与回放在内核里；写入口：运行时总开关关着回 7003）。
// 只给智能体工具用（CONTRACT §J2）；读已存的图见 GetPreview（REST GET /plans/:no/preview）。
func (sc *Scope) PreviewFrame(ctx context.Context, no string) (*core.PreviewView, error) {
	no, err := checkStr(no, "plan_no")
	if err != nil {
		return nil, err
	}
	return sc.s.PreviewFrame(sc.ctx(ctx), sc.uid, no)
}

// GetPreview 只读已存的定妆图并现签新地址（不出图、不占额度、不看总开关；没有 → 4001）。
func (sc *Scope) GetPreview(ctx context.Context, no string) (*core.PreviewView, error) {
	no, err := checkStr(no, "no")
	if err != nil {
		return nil, err
	}
	return sc.s.GetPreview(ctx, sc.uid, no)
}

// PatchAnnotations 改标注出新方案（限频 annotate；缺项 / 失败 / 同一改动的重放不占次数）。req.UserID 以登录用户为准。
func (sc *Scope) PatchAnnotations(ctx context.Context, req core.PatchReq, hasAnnotations bool) (*core.PlanView, error) {
	no, err := checkStr(req.PlanNo, "no")
	if err != nil {
		return nil, err
	}
	if !hasAnnotations {
		return nil, xerr.New(xerr.ParamMissing, "缺少 annotations")
	}
	refund, err := sc.take(ctx, ActionAnnotate)
	if err != nil {
		return nil, err
	}
	req.UserID, req.PlanNo = sc.uid, no
	v, created, err := sc.s.PatchAnnotations(sc.ctx(ctx), req)
	if err != nil || !created {
		refund() // 缺项 / 失败 / 同一改动的重放不占方案次数
	}
	return v, sc.done(err)
}

// SavePlan 保存到我的方案。
func (sc *Scope) SavePlan(ctx context.Context, no, name string) (*core.SavedPlanView, error) {
	no, err := checkStr(no, "no")
	if err != nil {
		return nil, err
	}
	v, err := sc.s.SavePlan(ctx, sc.uid, no, name)
	return v, sc.done(err)
}

// ListSavedPlans 我的方案（page 默认 1，size 默认 20；0 = 默认，负数不合法）。
func (sc *Scope) ListSavedPlans(ctx context.Context, page, size int) (*core.SavedPlanList, error) {
	page, size, err := pageOf(page, size)
	if err != nil {
		return nil, err
	}
	return sc.s.ListSavedPlans(ctx, sc.uid, page, size)
}

// DeleteSavedPlan 删除我的方案。
func (sc *Scope) DeleteSavedPlan(ctx context.Context, id int64) (map[string]any, error) {
	if err := checkID(id, "id"); err != nil {
		return nil, err
	}
	if err := sc.s.DeleteSavedPlan(ctx, sc.uid, id); err != nil {
		return nil, err
	}
	sc.kick()
	return map[string]any{"id": id}, nil
}

// ReuseSavedPlan 再做一条（限频 annotate；失败 / 同键重放不占次数）。req.UserID 以登录用户为准。
func (sc *Scope) ReuseSavedPlan(ctx context.Context, req core.ReuseReq) (*core.PlanView, error) {
	if err := checkID(req.SavedID, "id"); err != nil {
		return nil, err
	}
	refund, err := sc.take(ctx, ActionAnnotate)
	if err != nil {
		return nil, err
	}
	req.UserID = sc.uid
	v, created, err := sc.s.ReuseSavedPlan(sc.ctx(ctx), req)
	if err != nil || !created {
		refund() // 失败 / 同键重放不占方案次数
	}
	return v, sc.done(err)
}

// PublishCopy 发布文案（CONTRACT §N，免费、无同意卡；每人每天 N 次，同成片同平台回放不占次数，额度与回放在内核里）。写入口：运行时总开关关着回 7003。
func (sc *Scope) PublishCopy(ctx context.Context, jobID int64, platform string) (*core.PublishCopyView, error) {
	if err := checkID(jobID, "job_id"); err != nil {
		return nil, err
	}
	return sc.s.PublishCopy(sc.ctx(ctx), sc.uid, jobID, platform)
}

// SubmitFeedback 成片反馈（CONTRACT §Z，免费、不动账）。读入口口径：不受运行时总开关影响；只有轻量限频（每分钟 / 每日，防刷）。
// 覆盖更新与审核都在内核里。
func (sc *Scope) SubmitFeedback(ctx context.Context, req core.FeedbackReq) (*core.FeedbackView, error) {
	if err := checkID(req.JobID, "job_id"); err != nil {
		return nil, err
	}
	req.UserID = sc.uid
	refund, err := sc.take(ctx, ActionFeedback)
	if err != nil {
		return nil, err
	}
	v, err := sc.s.SubmitFeedback(sc.ctx(ctx), req)
	if err != nil {
		refund() // 失败不占次数
	}
	return v, err
}

// ClassifyRefs 素材识别（CONTRACT §AA，免费、只读、不动账）。写入口口径（总开关关着回 7003：它会调视觉模型，暂停期间不烧成本）；限频 classify，
// 请求被拒（参数 / 归属）或识别整体降级时退回本次额度。仅 REST 使用，智能体走对话里的附件，不经此入口。
func (sc *Scope) ClassifyRefs(ctx context.Context, analysisNo string, images []core.ClassifyImageIn) (*core.ClassifyView, error) {
	if analysisNo = strings.TrimSpace(analysisNo); analysisNo != "" {
		if _, err := checkStr(analysisNo, "analysisNo"); err != nil {
			return nil, err
		}
	}
	refund, err := sc.take(ctx, ActionClassify)
	if err != nil {
		return nil, err
	}
	v, err := sc.s.ClassifyRefs(sc.ctx(ctx), core.ClassifyReq{UserID: sc.uid, AnalysisNo: analysisNo, Images: images})
	if err != nil || allDegraded(v) {
		refund()
	}
	return v, err
}

func allDegraded(v *core.ClassifyView) bool {
	if v == nil || !v.Degraded {
		return false
	}
	for _, it := range v.Items {
		if it.Confidence > 0 {
			return false
		}
	}
	return true
}

// Wish 「想怎么改」翻译成改法草稿（CONTRACT §AA，免费、只读：不建方案、不预留、不扣点）。写入口口径（同上）；限频 wish，请求被拒或降级时退回额度。
// 仅 REST 使用（独立页）；智能体在对话里自己出方案，不经此入口。
func (sc *Scope) Wish(ctx context.Context, req core.WishReq) (*core.WishView, error) {
	no, err := checkStr(req.AnalysisNo, "no")
	if err != nil {
		return nil, err
	}
	req.AnalysisNo, req.UserID = no, sc.uid
	refund, err := sc.take(ctx, ActionWish)
	if err != nil {
		return nil, err
	}
	v, err := sc.s.Wish(sc.ctx(ctx), req)
	if err != nil || (v != nil && v.Degraded) {
		refund()
	}
	return v, err
}

// ExportRatio 多比例导出（CONTRACT §N，免费、无同意卡；每成片每比例每模式一次，结果自动入素材库）。写入口。
func (sc *Scope) ExportRatio(ctx context.Context, jobID int64, ratio, mode string) (*core.ExportView, error) {
	if err := checkID(jobID, "job_id"); err != nil {
		return nil, err
	}
	return sc.s.ExportRatio(sc.ctx(ctx), sc.uid, jobID, ratio, mode)
}

// RetryAudio 免费重做声音（CONTRACT §Q，不扣点、无同意卡；只对 result.audio=failed 的已交付成片可用，每成片最多 3 次，同 call_id 重放不重做）。写入口。
func (sc *Scope) RetryAudio(ctx context.Context, jobID int64, callID string) (*core.AudioRetryView, error) {
	if err := checkID(jobID, "job_id"); err != nil {
		return nil, err
	}
	return sc.s.RetryAudio(sc.ctx(ctx), sc.uid, jobID, callID)
}

// ShareSample 把完整成片分享成公开示例（CONTRACT §T，免费、不动账；建待审核投稿，一成片一条）。portraitConsent = 用户已勾选肖像授权
// （方案替换了人物时必须为 true）。写入口。
func (sc *Scope) ShareSample(ctx context.Context, jobID int64, portraitConsent bool) (*core.ShareView, error) {
	if err := checkID(jobID, "job_id"); err != nil {
		return nil, err
	}
	return sc.s.ShareSample(sc.ctx(ctx), sc.uid, jobID, portraitConsent)
}

// WithdrawSample 撤回自己分享的示例（CONTRACT §T）：待审核的作废，已上线的示例立即下架。写入口。
func (sc *Scope) WithdrawSample(ctx context.Context, submissionID int64) (*core.WithdrawView, error) {
	if err := checkID(submissionID, "submission_id"); err != nil {
		return nil, err
	}
	return sc.s.WithdrawSample(sc.ctx(ctx), sc.uid, submissionID)
}

// Exports 当前成片的已导出清单并现签新地址（只读，不看总开关；GET /jobs/:id/exports）。
func (sc *Scope) Exports(ctx context.Context, jobID int64) (*core.ExportList, error) {
	if err := checkID(jobID, "id"); err != nil {
		return nil, err
	}
	return sc.s.ListExports(ctx, sc.uid, jobID)
}

// BatchVariantArgs 批量的一个变体：替换主替换项的参考图。
type BatchVariantArgs struct{ RefImages []string }

func batchVariants(vs []BatchVariantArgs) []core.BatchVariant {
	out := make([]core.BatchVariant, len(vs))
	for i, v := range vs {
		out[i] = core.BatchVariant{RefImages: v.RefImages}
	}
	return out
}

// QuoteBatch 批量报价（CONTRACT §M，只读：不建方案、不动钱、不限频、不看总开关）。
func (sc *Scope) QuoteBatch(ctx context.Context, planNo string, variants []BatchVariantArgs) (*core.BatchQuoteView, error) {
	no, err := checkStr(planNo, "plan_no")
	if err != nil {
		return nil, err
	}
	return sc.s.QuoteBatch(ctx, sc.uid, no, batchVariants(variants))
}

// BatchConfirmArgs 批量确认的入参。
type BatchConfirmArgs struct {
	PlanNo         string
	Variants       []BatchVariantArgs
	ExpectedPoints *int64 // 合计点数，必填 >0
	IdempotencyKey string
}

// ConfirmBatch 批量确认（扣点，整批原子）：expectedPoints 必填 >0 → 重放直接返回已有批次（不限频不计次）→ 限频 confirm（一批算一次）→ 内核。
func (sc *Scope) ConfirmBatch(ctx context.Context, a BatchConfirmArgs) (*core.BatchView, error) {
	no, err := checkStr(a.PlanNo, "plan_no")
	if err != nil {
		return nil, err
	}
	pts, err := requirePoints(a.ExpectedPoints, "批量确认合计")
	if err != nil {
		return nil, err
	}
	if v, err := sc.s.ExistingBatch(ctx, sc.uid, a.IdempotencyKey); err != nil || v != nil {
		return v, sc.done(err)
	}
	refund, err := sc.take(ctx, ActionConfirm)
	if err != nil {
		return nil, err
	}
	v, err := sc.s.ConfirmBatch(sc.ctx(ctx), core.BatchConfirmReq{UserID: sc.uid, PlanNo: no, Variants: batchVariants(a.Variants),
		ExpectedPoints: pts, IdempotencyKey: a.IdempotencyKey})
	if err != nil || (v != nil && v.Replay) {
		refund() // 没提交成功（价变 / 审核 / 余额不足 / 整批回滚…）或只是重放：不占次数
	}
	return v, sc.done(err)
}

// GetBatch 取一批任务。
func (sc *Scope) GetBatch(ctx context.Context, id int64) (*core.BatchView, error) {
	if err := checkID(id, "batch_id"); err != nil {
		return nil, err
	}
	return sc.s.GetBatch(ctx, sc.uid, id)
}
