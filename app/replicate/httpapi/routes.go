package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

const prefix = "/api/v1/replicate"

// Register 挂 /api/v1/replicate/**（全部挂 Auth：用户 id 取自登录态）。m 为 nil（未开启）时每个接口回 7003「功能暂未开放」。
// 信封同其它 Go 接口（pkg/result）；方案缺项 code=1003 + data.gaps；409 类错误 code=4002，reason 原样（PLAN_STALE 等）。
func Register(server *rest.Server, auth rest.Middleware, m *Module) {
	h := &handlers{m: m}
	g, po, pu, pa, de := http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	r := func(method, path string, fn func(w http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error), write bool) rest.Route {
		return rest.Route{Method: method, Path: prefix + path, Handler: h.wrap(fn, write)}
	}
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{auth},
		r(g, "/limits", h.limits, false),
		r(po, "/analyses", h.createAnalysis, true),
		r(g, "/analyses/:no", h.getAnalysis, false),
		r(po, "/plans", h.createPlan, true),
		r(po, "/plans/:no/quote", h.quotePlan, true),
		r(po, "/plans/:no/confirm", h.confirm, true),
		r(g, "/jobs", h.jobs, false),
		r(g, "/jobs/:id", h.job, false),
		r(po, "/jobs/:id/continue", h.cont, true),
		r(po, "/jobs/:id/stop", h.stop, true),
		r(po, "/jobs/:id/cancel-rest", h.cancelRest, true),
		r(g, "/models", h.models, false),
		r(po, "/jobs/:id/segments/:k/redo", h.redo, true),
		r(po, "/jobs/:id/segments/:k/save", h.saveSegment, true),
		r(pu, "/pref", h.pref, true),
		// 示例 / 改标注 / 我的方案（library.go，Go_V1.0.41）：示例列表与试看只读，不计限频
		r(g, "/samples", h.samples, false),
		r(po, "/samples/:id/try", h.trySample, false),
		r(g, "/plans/:no", h.getPlan, false),
		r(pa, "/plans/:no/annotations", h.patchAnnotations, true),
		r(po, "/plans/:no/save", h.savePlan, true),
		r(g, "/saved-plans", h.savedPlans, false),
		r(de, "/saved-plans/:id", h.deleteSavedPlan, true),
		r(po, "/saved-plans/:id/reuse", h.reuseSavedPlan, true),
	))
}

type handlers struct{ m *Module }

func errNotReady() error { return xerr.New(xerr.NotReady, "功能暂未开放") }

// wrap 取内核与登录用户，写接口另查运行时总开关（关着回 7003；读接口照常，已有任务能看）。
func (h *handlers) wrap(fn func(w http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error), write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.m == nil || h.m.rt == nil {
			writeErr(w, r, errNotReady())
			return
		}
		uid := ctxdata.UserID(r.Context())
		if uid <= 0 {
			writeErr(w, r, xerr.New(xerr.Unauthorized, "请先登录"))
			return
		}
		s, enabled := h.m.rt.Service(r.Context())
		if s == nil || (write && !enabled) {
			writeErr(w, r, errNotReady())
			return
		}
		out, err := fn(w, r, s, uid)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		if write && h.m.taskSync != nil {
			h.m.taskSync.Kick() // 任务中心尽快看到新状态（轮询器另有周期同步兜底）
		}
		httpx.OkJsonCtx(r.Context(), w, out)
	}
}

// writeErr 方案缺项（*core.GapsError）→ code=1003 + data.gaps；其余走默认信封（业务码 + reason 原样）。
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		writeRateLimited(w, r, rl)
		return
	}
	var ge *core.GapsError
	if errors.As(err, &ge) {
		httpx.WriteJsonCtx(r.Context(), w, http.StatusOK, result.Body{Code: xerr.ParamError, Reason: xerr.Reason(xerr.ParamError),
			Msg: ge.Error(), Data: map[string]any{"gaps": ge.Gaps}})
		return
	}
	httpx.ErrorCtx(r.Context(), w, err)
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return xerr.Param("读取请求体失败")
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return xerr.Param("请求体不是合法 JSON")
	}
	return nil
}

// idemKey 写接口的幂等键：Idempotency-Key，兼容旧头 X-Request-Id（网关 idempotency 中间件两者同步）。
func idemKey(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		return k
	}
	return strings.TrimSpace(r.Header.Get("X-Request-Id"))
}

func pathInt(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(pathvar.Vars(r)[name], 10, 64)
	if err != nil || n <= 0 {
		return 0, xerr.Param(name + " 不合法")
	}
	return n, nil
}

func pathStr(r *http.Request, name string) (string, error) {
	v := strings.TrimSpace(pathvar.Vars(r)[name])
	if v == "" || len(v) > 64 {
		return "", xerr.Param(name + " 不合法")
	}
	return v, nil
}

func (h *handlers) limits(_ http.ResponseWriter, _ *http.Request, s *core.Service, _ int64) (any, error) {
	return s.Limits(), nil
}

func (h *handlers) createAnalysis(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	var body struct {
		SourceURL string `json:"sourceUrl"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body.SourceURL) == "" {
		return nil, xerr.New(xerr.ParamMissing, "请先上传要做同款的视频")
	}
	if len(strings.TrimSpace(body.SourceURL)) > 1024 {
		return nil, xerr.Param("视频地址太长了，请重新上传这条视频")
	}
	refund, err := h.take(r, ActionAnalysis, uid)
	if err != nil {
		return nil, err
	}
	v, err := s.CreateAnalysis(r.Context(), core.CreateAnalysisReq{UserID: uid, SourceURL: body.SourceURL})
	if err != nil || (v != nil && v.Cached) {
		refund() // 失败不计次；同一条视频复用已有分析（单飞）也不计次（EXPERIENCE 计次口径 F-22）
	}
	return v, err
}

// take 限频（ratelimit.go）：命中返回 *RateLimitError（写成 429）。
func (h *handlers) take(r *http.Request, action string, uid int64) (func(), error) {
	if h.m.limiter == nil {
		return func() {}, nil
	}
	return h.m.limiter.take(r.Context(), action, uid, h.m.rt.Rate())
}

func (h *handlers) getAnalysis(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return s.GetAnalysis(r.Context(), uid, no)
}

func (h *handlers) createPlan(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	var body struct {
		AnalysisNo  string           `json:"analysisNo"`
		SampleID    int64            `json:"sampleId"`
		Annotations core.Annotations `json:"annotations"`
		RefImages   []string         `json:"refImages"`
		ModelGroup  string           `json:"modelGroup"`
		Resolution  string           `json:"resolution"`
		AspectRatio string           `json:"aspectRatio"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body.AnalysisNo) == "" && body.SampleID <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少分析编号，请重新上传视频")
	}
	refund, err := h.take(r, ActionPlan, uid)
	if err != nil {
		return nil, err
	}
	v, err := s.CreatePlan(r.Context(), core.PlanReq{UserID: uid, AnalysisNo: body.AnalysisNo, Annotations: body.Annotations,
		RefImages: body.RefImages, ModelGroup: body.ModelGroup, Resolution: body.Resolution, AspectRatio: body.AspectRatio,
		SampleID: body.SampleID})
	if err != nil {
		refund() // 缺项 / 失败不占方案次数
	}
	return v, err
}

func (h *handlers) quotePlan(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return s.QuotePlan(r.Context(), uid, no)
}

func (h *handlers) confirm(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	var body struct {
		PlanHash       string `json:"planHash"`
		Stage          string `json:"stage"`
		ExpectedPoints *int64 `json:"expectedPoints"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	pts, err := requirePoints(body.ExpectedPoints, "确认")
	if err != nil {
		return nil, err
	}
	if body.Stage != "" && body.Stage != "first" && body.Stage != "all" {
		return nil, xerr.Param("stage 只能是 first 或 all")
	}
	// 重放 / 方案已确认：直接返回已有 job，不走限频、不计次
	if v, err := s.ExistingConfirm(r.Context(), uid, no, idemKey(r)); err != nil || v != nil {
		return v, err
	}
	refund, err := h.take(r, ActionConfirm, uid)
	if err != nil {
		return nil, err
	}
	v, err := s.Confirm(r.Context(), core.ConfirmReq{UserID: uid, PlanNo: no, PlanHash: body.PlanHash, Stage: body.Stage,
		IdempotencyKey: idemKey(r), ExpectedPoints: pts})
	if err != nil || (v != nil && v.Replay) {
		refund() // 没确认成功（审核没过 / 价变 / 余额不足…）或只是重放已有 job：不占次数
	}
	return v, err
}

func (h *handlers) job(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return s.JobView(r.Context(), uid, id)
}

// jobs GET /jobs?page=&size=：本人历史（创建倒序；page 从 1 起，size 默认 20、最大 50）。
func (h *handlers) jobs(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	q := r.URL.Query()
	page, size := 1, 20
	if v := strings.TrimSpace(q.Get("page")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, xerr.Param("page 不合法")
		}
		page = n
	}
	if v := strings.TrimSpace(q.Get("size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, xerr.Param("size 不合法")
		}
		size = n
	}
	return s.ListJobs(r.Context(), uid, page, size)
}

func (h *handlers) cont(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	var body struct {
		PlanHash       string `json:"planHash"`
		ExpectedPoints *int64 `json:"expectedPoints"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	pts, err := requirePoints(body.ExpectedPoints, "继续")
	if err != nil {
		return nil, err
	}
	return s.Continue(r.Context(), core.ContinueReq{UserID: uid, JobID: id, PlanHash: body.PlanHash,
		IdempotencyKey: idemKey(r), ExpectedPoints: pts})
}

func (h *handlers) stop(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return s.Stop(r.Context(), uid, id)
}

// cancelRest POST /jobs/{id}/cancel-rest：不做剩下的段（只取消没开始的段，在途段做完；幂等，返回新的 JobView）。
func (h *handlers) cancelRest(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return s.CancelRest(r.Context(), uid, id)
}

// models GET /models?analysisNo=&sampleId=&resolution=&aspectRatio=：可选出片模型（名称、特点、单价/秒、是否支持该时长与画幅）。
func (h *handlers) models(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	q := r.URL.Query()
	req := core.ModelsReq{UserID: uid, AnalysisNo: strings.TrimSpace(q.Get("analysisNo")), Resolution: q.Get("resolution"), AspectRatio: q.Get("aspectRatio")}
	if v := strings.TrimSpace(q.Get("sampleId")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, xerr.Param("sampleId 不合法")
		}
		req.SampleID = n
	}
	return s.ListModels(r.Context(), req)
}

func (h *handlers) redo(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	k, err := pathInt(r, "k")
	if err != nil {
		return nil, err
	}
	var body struct {
		ExpectedPoints *int64 `json:"expectedPoints"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	pts, err := requirePoints(body.ExpectedPoints, "重做")
	if err != nil {
		return nil, err
	}
	return s.Redo(r.Context(), core.RedoReq{UserID: uid, JobID: id, SegIndex: int(k), IdempotencyKey: idemKey(r), ExpectedPoints: pts})
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

func (h *handlers) pref(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	var body struct {
		AutoContinue *bool `json:"autoContinue"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.AutoContinue == nil {
		return nil, xerr.New(xerr.ParamMissing, "缺少 autoContinue")
	}
	if err := s.SetAutoContinue(r.Context(), uid, *body.AutoContinue); err != nil {
		return nil, err
	}
	return map[string]any{"autoContinue": *body.AutoContinue}, nil
}

// saveSegment POST /jobs/{id}/segments/{k}/save：把已做好的这一段保存到素材库（幂等；入库异步，结果看 library.status）。
func (h *handlers) saveSegment(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	k, err := pathInt(r, "k")
	if err != nil {
		return nil, err
	}
	return s.SaveSegment(r.Context(), uid, id, int(k))
}
