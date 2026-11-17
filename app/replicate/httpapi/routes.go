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
	r := func(method, path string, fn handlerFn, write bool) rest.Route {
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
		r(g, "/jobs/:id/exports", h.exports, false), // 当前成片的多比例导出清单（只读，现签新地址；§N）
		r(po, "/jobs/:id/continue", h.cont, true),
		r(po, "/jobs/:id/stop", h.stop, true),
		r(po, "/jobs/:id/cancel-rest", h.cancelRest, true),
		r(g, "/models", h.models, false),
		r(g, "/voices", h.voices, false), // 换声音的音色列表（只读；CONTRACT §P，?gender=&style=）
		r(po, "/jobs/:id/segments/:k/redo", h.redo, true),
		r(po, "/jobs/:id/feedback", h.feedback, false), // 成片反馈（CONTRACT §Z）：免费、不动账，不受运行时总开关影响，有轻量限频
		r(po, "/jobs/:id/segments/:k/save", h.saveSegment, true),
		r(pu, "/pref", h.pref, true),
		// 示例 / 改标注 / 我的方案（library.go，Go_V1.0.41）：示例列表与试看只读，不计限频
		r(g, "/samples", h.samples, false),
		r(po, "/samples/:id/try", h.trySample, false),
		r(g, "/plans/:no", h.getPlan, false),
		r(g, "/plans/:no/preview", h.getPreview, false), // 已存定妆图（只读，现签新地址；不出图不占额度）
		r(pa, "/plans/:no/annotations", h.patchAnnotations, true),
		r(po, "/plans/:no/save", h.savePlan, true),
		r(g, "/saved-plans", h.savedPlans, false),
		r(de, "/saved-plans/:id", h.deleteSavedPlan, true),
		r(po, "/saved-plans/:id/reuse", h.reuseSavedPlan, true),
	))
}

type handlers struct{ m *Module }

// handlerFn 薄 REST 处理器：只解析请求（路径 / 查询 / 请求体）→ 调 Scope（ops.go），不含业务口径。
type handlerFn = func(w http.ResponseWriter, r *http.Request, sc *Scope) (any, error)

func errNotReady() error { return xerr.New(xerr.NotReady, "功能暂未开放") }

// wrap 经 Ops.Enter 取内核与登录用户，写接口另查运行时总开关（关着回 7003；读接口照常，已有任务能看）。
func (h *handlers) wrap(fn handlerFn, write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, err := h.m.Ops().Enter(r.Context(), ctxdata.UserID(r.Context()), write)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		out, err := fn(w, r, sc)
		if err != nil {
			writeErr(w, r, err)
			return
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

// limitsView limits 加上运行时总开关：前端据此在页头说清「暂停新提交」，而不是等用户点了才报 7003。
type limitsView struct {
	core.LimitsView
	// Enabled 当前是否接新提交（afv_system_config.replicate.enabled，库里没有时取 Replicate.Enabled）
	Enabled bool `json:"enabled"`
}

func (h *handlers) limits(_ http.ResponseWriter, _ *http.Request, sc *Scope) (any, error) {
	return sc.Limits(), nil
}

func (h *handlers) createAnalysis(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	var body struct {
		SourceURL string `json:"sourceUrl"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return sc.CreateAnalysis(r.Context(), body.SourceURL)
}

func (h *handlers) getAnalysis(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return sc.GetAnalysis(r.Context(), no)
}

func (h *handlers) createPlan(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	var body struct {
		AnalysisNo  string           `json:"analysisNo"`
		SampleID    int64            `json:"sampleId"`
		Annotations core.Annotations `json:"annotations"`
		RefImages   []string         `json:"refImages"`
		ModelGroup  string           `json:"modelGroup"`
		Resolution  string           `json:"resolution"`
		AspectRatio string           `json:"aspectRatio"`
		Continuity  string           `json:"continuity"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return sc.CreatePlan(r.Context(), core.PlanReq{AnalysisNo: body.AnalysisNo, Annotations: body.Annotations,
		RefImages: body.RefImages, ModelGroup: body.ModelGroup, Resolution: body.Resolution, AspectRatio: body.AspectRatio,
		SampleID: body.SampleID, Continuity: body.Continuity})
}

func (h *handlers) quotePlan(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return sc.QuotePlan(r.Context(), no)
}

func (h *handlers) confirm(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
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
	return sc.Confirm(r.Context(), ConfirmArgs{PlanNo: no, PlanHash: body.PlanHash, Stage: body.Stage,
		ExpectedPoints: body.ExpectedPoints, IdempotencyKey: idemKey(r)})
}

func (h *handlers) job(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return sc.JobView(r.Context(), id)
}

// exports GET /jobs/{id}/exports：当前成片已导出的各比例（只读，仅本人；成片未交付 / 没导出过 = 空数组）。
func (h *handlers) exports(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return sc.Exports(r.Context(), id)
}

// jobs GET /jobs?page=&size=：本人历史（创建倒序；page 从 1 起，size 默认 20、最大 50）。
func (h *handlers) jobs(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	page, size, err := pageQuery(r)
	if err != nil {
		return nil, err
	}
	return sc.ListJobs(r.Context(), page, size)
}

// pageQuery 解析 ?page=&size=：不传 = 0（Ops 取默认），传了必须是正整数。
func pageQuery(r *http.Request) (page, size int, err error) {
	q := r.URL.Query()
	for _, p := range []struct {
		name string
		dst  *int
	}{{"page", &page}, {"size", &size}} {
		if v := strings.TrimSpace(q.Get(p.name)); v != "" {
			n, perr := strconv.Atoi(v)
			if perr != nil || n <= 0 {
				return 0, 0, xerr.Param(p.name + " 不合法")
			}
			*p.dst = n
		}
	}
	return page, size, nil
}

func (h *handlers) cont(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
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
	return sc.Continue(r.Context(), ContinueArgs{JobID: id, PlanHash: body.PlanHash, ExpectedPoints: body.ExpectedPoints, IdempotencyKey: idemKey(r)})
}

func (h *handlers) stop(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return sc.Stop(r.Context(), id)
}

// cancelRest POST /jobs/{id}/cancel-rest：不做剩下的段（只取消没开始的段，在途段做完；幂等，返回新的 JobView）。
func (h *handlers) cancelRest(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return sc.CancelRest(r.Context(), id)
}

// models GET /models?analysisNo=&sampleId=&resolution=&aspectRatio=：可选出片模型（名称、特点、单价/秒、是否支持该时长与画幅）。
func (h *handlers) models(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	q := r.URL.Query()
	req := core.ModelsReq{AnalysisNo: strings.TrimSpace(q.Get("analysisNo")), Resolution: q.Get("resolution"), AspectRatio: q.Get("aspectRatio")}
	if v := strings.TrimSpace(q.Get("sampleId")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, xerr.Param("sampleId 不合法")
		}
		req.SampleID = n
	}
	return sc.Models(r.Context(), req)
}

// voices GET /voices?gender=&style=：可选配音音色（只读）。样音地址是音色目录里的试听地址（公有桶、无签名，原样返回）。
func (h *handlers) voices(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	q := r.URL.Query()
	return sc.ListVoices(r.Context(), q.Get("gender"), q.Get("style"))
}

func (h *handlers) redo(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
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
	return sc.Redo(r.Context(), RedoArgs{JobID: id, SegIndex: k, ExpectedPoints: body.ExpectedPoints, IdempotencyKey: idemKey(r)})
}

func (h *handlers) pref(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	var body struct {
		AutoContinue *bool `json:"autoContinue"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return sc.SetAutoContinue(r.Context(), body.AutoContinue)
}

// saveSegment POST /jobs/{id}/segments/{k}/save：把已做好的这一段保存到素材库（幂等；入库异步，结果看 library.status）。
func (h *handlers) saveSegment(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	k, err := pathInt(r, "k")
	if err != nil {
		return nil, err
	}
	return sc.SaveSegment(r.Context(), id, k)
}

// feedback POST /jobs/:id/feedback：成片反馈（CONTRACT §Z）。segIndex 不填 = 整条成片；reasons 只在 rating=down 时收。
func (h *handlers) feedback(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	var body struct {
		Rating   string   `json:"rating"`
		Reasons  []string `json:"reasons"`
		SegIndex int      `json:"segIndex"`
		Note     string   `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return sc.SubmitFeedback(r.Context(), core.FeedbackReq{JobID: id, Rating: body.Rating, Reasons: body.Reasons, SegIndex: body.SegIndex, Note: body.Note})
}
