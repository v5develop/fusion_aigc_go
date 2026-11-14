// Package plaza 广场模板域的用户端原生接口（/api/v1/plaza/*）。
//
// 手写路由（不走 gateway.api 的 goctl 生成）：响应由 pkg/v1wire 按 Java CommonResult 的形状直接写出，
// 不经 go-zero 的 httpx 信封，也不被 jsoncase 改写（jsoncase 对 /api/v1/plaza/ 放行）。
// 严格垂直：除行业目录、偏好、行业建议、按 id 取单条外，每个接口都要求 industry，缺则 HTTP 400（PARAM_MISSING）。
// 设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 第 5.1、16、17 节。
package plaza

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/plaza/core"
	"aigc-local-life/pkg/clientip"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/v1wire"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

const prefix = "/api/v1/plaza"

// Register 注册用户端路由。偏好 / 最近用过 / 收藏 / 我的模板挂 Auth 中间件；其余公开（未登录同样必须带 industry）。
func Register(server *rest.Server, c *svc.ServiceContext) {
	h := &handlers{c: c}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodGet, Path: prefix + "/industries", Handler: h.industries},
		{Method: http.MethodGet, Path: prefix + "/counts", Handler: h.counts},
		{Method: http.MethodGet, Path: prefix + "/templates", Handler: h.templates},
		{Method: http.MethodGet, Path: prefix + "/templates/:id", Handler: h.templateByID},
		{Method: http.MethodPost, Path: prefix + "/templates/:id/prepare", Handler: h.prepare},
		{Method: http.MethodGet, Path: prefix + "/samples", Handler: h.samples},
		{Method: http.MethodGet, Path: prefix + "/quote", Handler: h.quote},
		{Method: http.MethodPost, Path: prefix + "/industry-suggestions", Handler: h.suggest},
		{Method: http.MethodGet, Path: prefix + "/nodes", Handler: h.nodes},
	})
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.Auth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/recent", Handler: h.recent},
		rest.Route{Method: http.MethodGet, Path: prefix + "/industry-preference", Handler: h.getPref},
		rest.Route{Method: http.MethodPut, Path: prefix + "/industry-preference", Handler: h.putPref},
		rest.Route{Method: http.MethodPost, Path: prefix + "/favorites/:templateId", Handler: h.addFavorite},
		rest.Route{Method: http.MethodDelete, Path: prefix + "/favorites/:templateId", Handler: h.removeFavorite},
		rest.Route{Method: http.MethodGet, Path: prefix + "/favorites", Handler: h.listFavorites},
		rest.Route{Method: http.MethodPost, Path: prefix + "/my-templates", Handler: h.createMyTemplate},
		rest.Route{Method: http.MethodGet, Path: prefix + "/my-templates", Handler: h.listMyTemplates},
		rest.Route{Method: http.MethodPut, Path: prefix + "/my-templates/:id", Handler: h.updateMyTemplate},
		rest.Route{Method: http.MethodDelete, Path: prefix + "/my-templates/:id", Handler: h.deleteMyTemplate},
		rest.Route{Method: http.MethodPost, Path: prefix + "/overlays", Handler: h.createOverlay},
		rest.Route{Method: http.MethodGet, Path: prefix + "/overlays", Handler: h.listOverlays},
		rest.Route{Method: http.MethodGet, Path: prefix + "/overlays/:id", Handler: h.getOverlay},
		rest.Route{Method: http.MethodPut, Path: prefix + "/overlays/:id", Handler: h.updateOverlay},
		rest.Route{Method: http.MethodPost, Path: prefix + "/overlay-intents", Handler: h.createOverlayIntent},
	))
}

type handlers struct{ c *svc.ServiceContext }

func (h *handlers) svc(w http.ResponseWriter, r *http.Request) *core.Service {
	if h.c.Plaza == nil {
		v1wire.Fail(w, r, core.ErrNotReady())
		return nil
	}
	return h.c.Plaza
}

func (h *handlers) industries(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	d, err := s.Industries(r.Context())
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.Public(w, 60) // 目录：公开缓存 60 秒
	v1wire.OK(w, r, d)
}

func (h *handlers) counts(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	out, err := s.Counts(r.Context(), q.Get("industry"), q.Get("subIndustry"))
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) templates(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	p, err := core.ParseListParams(r.URL.Query())
	if err != nil {
		// 缺 industry 比参数格式错优先（前端据此回行业门）
		if strings.TrimSpace(p.Industry) == "" {
			err = core.ErrIndustryRequired()
		}
		v1wire.Fail(w, r, err)
		return
	}
	p.IPHash = s.HashIP(clientIP(r))
	out, err := s.Templates(r.Context(), p)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) templateByID(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := strconv.ParseInt(pathvar.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		v1wire.Fail(w, r, xerr.Param("id 不合法"))
		return
	}
	out, err := s.TemplateByID(r.Context(), id)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

// prepare 做同款准备：按模板的入参 schema 校验并规整入参（必填、数量、长度、范围、选项；visibleWhen 不可见项忽略）。
func (h *handlers) prepare(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := strconv.ParseInt(pathvar.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		v1wire.Fail(w, r, xerr.Param("id 不合法"))
		return
	}
	var body struct {
		Inputs map[string]json.RawMessage `json:"inputs"`
	}
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	out, err := s.PrepareInputs(r.Context(), id, body.Inputs)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, map[string]any{"inputs": out})
}

func (h *handlers) samples(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	tid, err := optInt64(q.Get("templateId"))
	if err != nil {
		v1wire.Fail(w, r, xerr.Param("templateId 不合法"))
		return
	}
	out, err := s.Samples(r.Context(), q.Get("industry"), q.Get("subIndustry"), tid)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, map[string]any{"samples": out})
}

func (h *handlers) quote(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	tid, err := optInt64(q.Get("templateId"))
	if err != nil {
		v1wire.Fail(w, r, xerr.Param("templateId 不合法"))
		return
	}
	variants, err := optInt64(q.Get("variants"))
	if err != nil {
		v1wire.Fail(w, r, xerr.Param("variants 不合法"))
		return
	}
	var refImages *int
	if raw := q.Get("refImageCount"); raw != "" {
		n, err := optInt64(raw)
		if err != nil || n < 0 {
			v1wire.Fail(w, r, xerr.Param("refImageCount 不合法"))
			return
		}
		if n > 50 {
			n = 50
		}
		c := int(n)
		refImages = &c
	}
	p := core.QuoteParams{
		RefImageCount: refImages,
		Resolution:    q.Get("resolution"),
		Industry:      q.Get("industry"), Sub: q.Get("subIndustry"), TemplateID: tid, Variants: int(variants),
		IPHash: s.HashIP(clientIP(r)), Bearer: r.Header.Get("Authorization"),
	}
	out, err := s.Quote(r.Context(), p)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	if !out.Personalized { // 匿名标价才可公开缓存；个性化报价含余额，不能缓存
		v1wire.Public(w, 60)
	} else {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) suggest(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	var body struct {
		Text string `json:"text"`
		Sid  string `json:"sid"`
	}
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	// 匿名可提交；已登录时记录 user_id（这个路由不挂 Auth，所以尽力解析令牌，解析不了就当匿名）
	uid := h.optionalUserID(r)
	err := s.SubmitSuggestion(r.Context(), core.SuggestionInput{Text: body.Text, Sid: body.Sid, UserID: uid, IPHash: s.HashIP(clientIP(r))})
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, map[string]any{"accepted": true})
}

func (h *handlers) recent(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	limit, err := optInt64(q.Get("limit"))
	if err != nil {
		v1wire.Fail(w, r, xerr.Param("limit 不合法"))
		return
	}
	out, err := s.Recent(r.Context(), ctxdata.UserID(r.Context()), q.Get("industry"), q.Get("subIndustry"), strings.TrimSpace(q.Get("mediaType")), int(limit))
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, map[string]any{"items": out})
}

func (h *handlers) getPref(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	out, err := s.GetPreference(r.Context(), ctxdata.UserID(r.Context()))
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) putPref(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	var body struct {
		IndustryIDs       []int64 `json:"industryIds"`
		PrimaryIndustryID int64   `json:"primaryIndustryId"`
	}
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	out, err := s.PutPreference(r.Context(), ctxdata.UserID(r.Context()), body.IndustryIDs, body.PrimaryIndustryID)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

// optionalUserID 尽力从 Authorization 解出用户 id；没有 / 解析失败 → 0（匿名）。
func (h *handlers) optionalUserID(r *http.Request) int64 {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if raw == "" || h.c.Session == nil || !h.c.Session.Enabled() {
		return 0
	}
	claims, err := h.c.Session.Parse(r.Context(), raw)
	if err != nil || claims == nil {
		return 0
	}
	return claims.UserID
}

// clientIP 调用方 IP：只信最近一跳可信代理写的头（X-Forwarded-For 首段可伪造），见 pkg/clientip。
func clientIP(r *http.Request) string { return clientip.From(r) }

func optInt64(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
