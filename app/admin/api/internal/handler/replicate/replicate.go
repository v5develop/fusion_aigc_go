// Package replicate 视频做同款后台接口（/api/v1/replicate/admin/**）：任务检索、详情、按段人工退点、强制收尾、总开关。
//
// 手写路由（不走 admin.api 的 goctl 生成），响应沿用 Go 默认信封（pkg/result）。业务在 app/replicate/admin。
// 权限：前缀映射 replicate:view（GET）；退点 replicate:refund、强制收尾 replicate:force、示例写 replicate:sample 走路由级覆盖
// （middleware.replicateRoutePerm，迁移 Go_V1.0.39 / Go_V1.0.41）。幂等键只认 Idempotency-Key（没带由业务派生）。契约：docs/api/api-replicate.md §9、§11。
package replicate

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/admin/api/internal/middleware"
	"aigc-local-life/app/admin/api/internal/svc"
	radmin "aigc-local-life/app/replicate/admin"
	replicatehttp "aigc-local-life/app/replicate/httpapi"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

const prefix = "/api/v1/replicate/admin"

// Register 注册后台路由（全部挂 AdminAuth）。
func Register(server *rest.Server, c *svc.ServiceContext) {
	h := &handlers{s: c.ReplicateAdmin}
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/jobs", Handler: h.wrap(h.list)},
		rest.Route{Method: http.MethodGet, Path: prefix + "/jobs/:id", Handler: h.wrap(h.detail)},
		rest.Route{Method: http.MethodPost, Path: prefix + "/jobs/:id/refund", Handler: h.wrap(h.refund)},
		rest.Route{Method: http.MethodPost, Path: prefix + "/jobs/:id/force-finish", Handler: h.wrap(h.force)},
	))
	registerSwitch(server, c)
	registerSamples(server, c, h)
}

type handlers struct{ s *radmin.Service }

func (h *handlers) wrap(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.s == nil {
			httpx.ErrorCtx(r.Context(), w, xerr.New(xerr.NotReady, "功能暂未开放"))
			return
		}
		out, err := fn(r)
		var pe *radmin.PartialError
		if errors.As(err, &pe) {
			// 部分单没退成：错误码 5001，同时下发逐单明细（哪些退了、哪些没退、为什么）
			httpx.WriteJsonCtx(r.Context(), w, http.StatusOK, result.Body{Code: xerr.ServerError, Reason: xerr.Reason(xerr.ServerError),
				Msg: pe.Msg, Data: pe.Resp})
			return
		}
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}
		httpx.OkJsonCtx(r.Context(), w, out)
	}
}

var cst = time.FixedZone("CST", 8*3600)

// parseTime RFC3339 / "2006-01-02 15:04:05" / "2006-01-02"（北京时间）；日期形式的 to 取当天结束（次日 0 点，左闭右开）。
func parseTime(raw string, end bool) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, cst); err == nil {
		return &t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", raw, cst)
	if err != nil {
		return nil, err
	}
	if end {
		t = t.AddDate(0, 0, 1)
	}
	return &t, nil
}

func optInt(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func (h *handlers) list(r *http.Request) (any, error) {
	q := r.URL.Query()
	var req radmin.ListReq
	var err error
	if req.UserID, err = optInt(q.Get("userId")); err != nil {
		return nil, xerr.Param("userId 不合法")
	}
	if req.JobID, err = optInt(q.Get("jobId")); err != nil {
		return nil, xerr.Param("jobId 不合法")
	}
	page, err := optInt(q.Get("page"))
	if err != nil || page < 0 {
		return nil, xerr.Param("page 不合法")
	}
	size, err := optInt(q.Get("size"))
	if err != nil || size < 0 {
		return nil, xerr.Param("size 不合法")
	}
	req.Page, req.Size = int(page), int(size)
	if req.From, err = parseTime(q.Get("from"), false); err != nil {
		return nil, xerr.Param("from 不合法")
	}
	if req.To, err = parseTime(q.Get("to"), true); err != nil {
		return nil, xerr.Param("to 不合法")
	}
	req.Status, req.TaskID, req.PlanNo = q.Get("status"), q.Get("taskId"), q.Get("planNo")
	return h.s.List(r.Context(), req)
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(pathvar.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		return 0, xerr.Param("id 不合法")
	}
	return id, nil
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
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

// idemKey 动钱 / 收尾的幂等键只认 Idempotency-Key（不回落 X-Request-Id：那是链路追踪号，每次请求都变）；
// 没带时由业务按 段 + 原因 + 当前预留单号集合（收尾：当前未结镜像集合）派生。
func idemKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

func (h *handlers) detail(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	return h.s.Detail(r.Context(), id)
}

func (h *handlers) refund(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		SegIndexes     []int  `json:"segIndexes"`
		Reason         string `json:"reason"`
		ExpectedPoints *int64 `json:"expectedPoints"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.ExpectedPoints == nil {
		return nil, xerr.New(xerr.ParamMissing, "缺少 expectedPoints（页面展示的可退合计）")
	}
	return h.s.Refund(r.Context(), radmin.RefundReq{JobID: id, AdminID: ctxdata.AdminID(r.Context()), SegIndexes: body.SegIndexes,
		Reason: body.Reason, ExpectedPoints: *body.ExpectedPoints, IdempotencyKey: idemKey(r)})
}

func (h *handlers) force(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return h.s.ForceFinish(r.Context(), radmin.ForceReq{JobID: id, AdminID: ctxdata.AdminID(r.Context()), Reason: body.Reason,
		IdempotencyKey: idemKey(r)})
}

// ---------------------------------------------------------------- 示例视频管理（Go_V1.0.41，契约 docs/api/api-replicate.md §11）
//
// 读 = replicate:view（中间件前缀映射）；写（新建 / 修改 / 删除 / 触发预分析）= replicate:sample：中间件 replicateRoutePerm 已对
// /samples/** 的非 GET 路由级覆盖为 replicate:sample，这里再查一次同一个码（纵深防御，路由表改动时不至于放空）。

const permSample = "replicate:sample"

func registerSamples(server *rest.Server, c *svc.ServiceContext, h *handlers) {
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/samples", Handler: h.wrap(h.sampleList)},
		rest.Route{Method: http.MethodGet, Path: prefix + "/samples/:id", Handler: h.wrap(h.sampleDetail)},
		rest.Route{Method: http.MethodPost, Path: prefix + "/samples", Handler: h.wrap(needSample(h.sampleCreate))},
		rest.Route{Method: http.MethodPut, Path: prefix + "/samples/:id", Handler: h.wrap(needSample(h.sampleUpdate))},
		rest.Route{Method: http.MethodDelete, Path: prefix + "/samples/:id", Handler: h.wrap(needSample(h.sampleDelete))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/samples/:id/analyze", Handler: h.wrap(needSample(h.sampleAnalyze))},
	))
}

// needSample 写示例要 replicate:sample（与中间件同一套 RBAC 判断）。
func needSample(fn func(r *http.Request) (any, error)) func(r *http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		if err := middleware.Require(r.Context(), permSample); err != nil {
			return nil, err
		}
		return fn(r)
	}
}

func (h *handlers) sampleList(r *http.Request) (any, error) {
	q := r.URL.Query()
	req := radmin.SampleListReq{Industry: q.Get("industry")}
	switch strings.TrimSpace(q.Get("enabled")) {
	case "":
	case "1", "true":
		on := true
		req.Enabled = &on
	case "0", "false":
		off := false
		req.Enabled = &off
	default:
		return nil, xerr.Param("enabled 只能是 0 或 1")
	}
	page, err := optInt(q.Get("page"))
	if err != nil || page < 0 {
		return nil, xerr.Param("page 不合法")
	}
	size, err := optInt(q.Get("size"))
	if err != nil || size < 0 {
		return nil, xerr.Param("size 不合法")
	}
	req.Page, req.Size = int(page), int(size)
	return h.s.ListSamples(r.Context(), req)
}

func (h *handlers) sampleDetail(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	return h.s.GetSample(r.Context(), id)
}

type sampleBody struct {
	Title       string          `json:"title"`
	Industry    string          `json:"industry"`
	SourceRid   string          `json:"sourceRid"`
	CoverRid    string          `json:"coverRid"`
	ResultRid   string          `json:"resultRid"`
	Annotations json.RawMessage `json:"annotations"`
	SortOrder   int             `json:"sortOrder"`
	Enabled     bool            `json:"enabled"`
	Analyze     bool            `json:"analyze"`
}

func (b sampleBody) req(r *http.Request, id int64) radmin.SampleReq {
	return radmin.SampleReq{ID: id, AdminID: ctxdata.AdminID(r.Context()), Title: b.Title, Industry: b.Industry, SourceRid: b.SourceRid,
		CoverRid: b.CoverRid, ResultRid: b.ResultRid, Annotations: b.Annotations, SortOrder: b.SortOrder, Enabled: b.Enabled, Analyze: b.Analyze}
}

func (h *handlers) sampleCreate(r *http.Request) (any, error) {
	var body sampleBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return h.s.CreateSample(r.Context(), body.req(r, 0))
}

func (h *handlers) sampleUpdate(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	var body sampleBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return h.s.UpdateSample(r.Context(), body.req(r, id))
}

func (h *handlers) sampleDelete(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	if err := h.s.DeleteSample(r.Context(), id, ctxdata.AdminID(r.Context())); err != nil {
		return nil, err
	}
	return map[string]any{"id": id}, nil
}

func (h *handlers) sampleAnalyze(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	return h.s.AnalyzeSample(r.Context(), id, ctxdata.AdminID(r.Context()))
}

// ---------------------------------------------------------------- 总开关（afv_system_config.replicate.enabled，数据库 > 文件配置）
//
// GET    /switch  当前开关（replicate:view）
// PUT    /switch  {enabled, reason} 写库，从此以库为准（replicate:switch）
// DELETE /switch  {reason} 恢复跟随部署配置（replicate:switch）

func registerSwitch(server *rest.Server, c *svc.ServiceContext) {
	sw := c.ReplicateSwitch
	wrap := func(fn func(r *http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if sw == nil {
				httpx.ErrorCtx(r.Context(), w, xerr.New(xerr.NotReady, "功能暂未开放"))
				return
			}
			out, err := fn(r)
			if err != nil {
				httpx.ErrorCtx(r.Context(), w, err)
				return
			}
			httpx.OkJsonCtx(r.Context(), w, out)
		}
	}
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/switch", Handler: wrap(func(r *http.Request) (any, error) {
			return sw.Get(r.Context())
		})},
		rest.Route{Method: http.MethodPut, Path: prefix + "/switch", Handler: wrap(func(r *http.Request) (any, error) {
			var body struct {
				Enabled *bool  `json:"enabled"`
				Reason  string `json:"reason"`
			}
			if err := decode(r, &body); err != nil {
				return nil, err
			}
			if body.Enabled == nil {
				return nil, xerr.New(xerr.ParamMissing, "缺少 enabled")
			}
			return sw.Set(r.Context(), replicatehttp.SwitchReq{AdminID: ctxdata.AdminID(r.Context()), Enabled: *body.Enabled, Reason: body.Reason})
		})},
		rest.Route{Method: http.MethodDelete, Path: prefix + "/switch", Handler: wrap(func(r *http.Request) (any, error) {
			var body struct {
				Reason string `json:"reason"`
			}
			if err := decode(r, &body); err != nil {
				return nil, err
			}
			return sw.Reset(r.Context(), ctxdata.AdminID(r.Context()), body.Reason)
		})},
	))
}
