// Package httpapi 「本周头条」的 HTTP 路由：用户端 GET /api/v1/plaza/headlines（gateway-api）与
// 运营端 /api/v1/plaza/admin/headlines/**（admin-api）。不走 goctl 生成（避免与行业模块同改 .api / routes.go），
// 由进程 main 手工挂载：
//
//	gateway-api：hl := httpapi.MustBuild(c.DataSource, sessRedis, c.PlazaHeadline, false, c.Plaza.AdminWriteEnabled); httpapi.RegisterUser(server, hl)
//	admin-api：  hl := httpapi.MustBuild(c.DataSource, sessRedis, c.PlazaHeadline, true, c.Plaza.AdminWriteEnabled);  httpapi.RegisterAdmin(server, ctx.AdminAuth, hl)
//	             PlazaHeadline.Jobs=true 时 go hl.Jobs.Start() / defer hl.Jobs.Stop()（多副本靠行租约选主；默认关）
//
// 后台权限：admin 中间件 prefixPerms 需要一行 {"/api/v1/plaza/admin", "creaft_catalog:view", "creaft_catalog:edit"}
// （GET=view、其余=edit；与行业模块共用同一前缀）。
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/service"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/clientip"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

// Headline 组装好的头条模块。
type Headline struct {
	Svc  *service.Service
	Jobs *service.Jobs // withJobs=false 或未配库时为 nil
}

// MustBuild 由数据源与 Redis 组装。dataSource 为空时服务返回「功能暂未开放」；sessRedis 为会话 Redis（可为 nil），
// conf.Redis.Addr 配了则用独立 Redis。adminWriteEnabled 即 Plaza.AdminWriteEnabled：为 false（Java 仍是模板写者）时
// 模板可见性同时要求 enabled=1，与 app/plaza/core 的用户端口径一致。
func MustBuild(dataSource string, sessRedis redis.UniversalClient, conf service.Conf, withJobs, adminWriteEnabled bool) *Headline {
	var repo *store.Repo
	if dataSource != "" {
		db, err := bundb.New(sqlx.NewMysql(dataSource))
		logx.Must(err)
		repo = store.New(db)
	} else {
		repo = &store.Repo{}
	}
	repo.RequireEnabled = !adminWriteEnabled
	rdb := sessRedis
	if conf.Redis.Addr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: conf.Redis.Addr, Password: conf.Redis.Password, DB: conf.Redis.DB})
	}
	svc := service.New(repo, signals.New(rdb), nil, conf)
	h := &Headline{Svc: svc}
	if withJobs && conf.Jobs && dataSource != "" {
		h.Jobs = service.NewJobs(svc)
	}
	return h
}

// ---------------------------------------------------------------- 用户端

// RegisterUser 挂用户端路由（公开，不挂 AuthMiddleware；industry 必填）。
func RegisterUser(server *rest.Server, h *Headline) {
	server.AddRoutes([]rest.Route{{Method: http.MethodGet, Path: "/api/v1/plaza/headlines", Handler: UserHeadlinesHandler(h.Svc)}})
}

// UserHeadlinesHandler GET /api/v1/plaza/headlines?industry=&subIndustry=&keyword=
// 缺 industry → HTTP 400 + PARAM_MISSING；行业失效 → HTTP 200 + INDUSTRY_NOT_FOUND。Cache-Control: public, max-age=30。
func UserHeadlinesHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		keyword := strings.TrimSpace(q.Get("keyword"))
		if len([]rune(keyword)) > 50 {
			httpx.ErrorCtx(r.Context(), w, xerr.Param("keyword 最多 50 字"))
			return
		}
		data, err := svc.Headlines(r.Context(), q.Get("industry"), q.Get("subIndustry"), keyword)
		if err != nil {
			if code, msg, ok := xerr.Parse(err); ok && code == xerr.ParamMissing {
				httpx.WriteJsonCtx(r.Context(), w, http.StatusBadRequest,
					map[string]any{"code": code, "reason": xerr.Reason(code), "msg": msg, "data": nil})
				return
			}
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=30")
		httpx.OkJsonCtx(r.Context(), w, data)
	}
}

// ---------------------------------------------------------------- 运营端

const adminPrefix = "/api/v1/plaza/admin/headlines"

// RegisterAdmin 挂运营端路由（AdminAuth 中间件；写操作成功后同步触发刷新，响应带 {version, refreshed}）。
func RegisterAdmin(server *rest.Server, auth rest.Middleware, h *Headline) {
	a := &admin{svc: h.Svc}
	routes := []rest.Route{
		{Method: http.MethodGet, Path: "/scopes", Handler: a.scopes},
		{Method: http.MethodGet, Path: "/current", Handler: a.current},
		{Method: http.MethodGet, Path: "/compare", Handler: a.compare},
		{Method: http.MethodGet, Path: "/templates/:id/headline-status", Handler: a.templateStatus},
		{Method: http.MethodGet, Path: "/snapshots", Handler: a.snapshots},
		{Method: http.MethodGet, Path: "/snapshots/at", Handler: a.snapshotAt},
		{Method: http.MethodGet, Path: "/snapshots/:version", Handler: a.snapshot},
		{Method: http.MethodPost, Path: "/refresh", Handler: a.refresh},
		{Method: http.MethodPost, Path: "/preview", Handler: a.preview},
		{Method: http.MethodGet, Path: "/params", Handler: a.params},
		{Method: http.MethodPut, Path: "/params", Handler: a.saveParams},
		{Method: http.MethodGet, Path: "/rules", Handler: a.rules},
		{Method: http.MethodPost, Path: "/rules", Handler: a.createRule},
		{Method: http.MethodPut, Path: "/rules/:id", Handler: a.updateRule},
		{Method: http.MethodDelete, Path: "/rules/:id", Handler: a.deleteRule},
		{Method: http.MethodPost, Path: "/position", Handler: a.position},
		{Method: http.MethodPost, Path: "/why", Handler: a.why},
		{Method: http.MethodPost, Path: "/suppress", Handler: a.suppress},
		{Method: http.MethodGet, Path: "/suppressions", Handler: a.suppressions},
		{Method: http.MethodDelete, Path: "/suppressions/:id", Handler: a.unsuppress},
	}
	if auth != nil {
		server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{auth}, routes...), rest.WithPrefix(adminPrefix))
	} else {
		server.AddRoutes(routes, rest.WithPrefix(adminPrefix))
	}
}

type admin struct{ svc *service.Service }

func meta(r *http.Request) service.Meta {
	ip := clientip.From(r) // 只信最近一跳可信代理写的头，见 pkg/clientip
	return service.Meta{OperatorID: ctxdata.AdminID(r.Context()), RequestID: r.Header.Get("X-Request-Id"), ClientIP: ip}
}

func reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		httpx.ErrorCtx(r.Context(), w, err)
		return
	}
	httpx.OkJsonCtx(r.Context(), w, v)
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(pathvar.Vars(r)[name], 10, 64)
	if err != nil || id <= 0 {
		return 0, xerr.Param(name + " 非法")
	}
	return id, nil
}

func intQuery(r *http.Request, key string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return v
	}
	return def
}

func parseAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, s, algo.CST); err == nil {
			return t, nil
		}
	}
	return time.Time{}, xerr.Param("时间格式应为 yyyy-MM-ddTHH:mm:ss")
}

type page[T any] struct {
	Total int `json:"total"`
	List  []T `json:"list"`
}

func (a *admin) scopes(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Scopes(r.Context())
	reply(w, r, v, err)
}

func (a *admin) current(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Current(r.Context(), r.URL.Query().Get("scope"))
	reply(w, r, v, err)
}

func (a *admin) compare(w http.ResponseWriter, r *http.Request) {
	var at time.Time
	if s := r.URL.Query().Get("at"); s != "" {
		var err error
		if at, err = parseAt(s); err != nil {
			reply(w, r, nil, err)
			return
		}
	}
	v, err := a.svc.Compare(r.Context(), strings.Split(r.URL.Query().Get("scopes"), ","), at)
	reply(w, r, v, err)
}

func (a *admin) templateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	v, err := a.svc.TemplateStatus(r.Context(), id)
	reply(w, r, v, err)
}

func (a *admin) snapshots(w http.ResponseWriter, r *http.Request) {
	list, total, err := a.svc.SnapshotList(r.Context(), intQuery(r, "page", 1), intQuery(r, "size", 20))
	reply(w, r, page[service.SnapshotMeta]{Total: total, List: list}, err)
}

func (a *admin) snapshot(w http.ResponseWriter, r *http.Request) {
	v, err := pathID(r, "version")
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	out, err := a.svc.SnapshotScope(r.Context(), v, r.URL.Query().Get("scope"))
	reply(w, r, out, err)
}

func (a *admin) snapshotAt(w http.ResponseWriter, r *http.Request) {
	at, err := parseAt(r.URL.Query().Get("time"))
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	out, err := a.svc.SnapshotAt(r.Context(), at, r.URL.Query().Get("scope"))
	reply(w, r, out, err)
}

func (a *admin) refresh(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.RefreshNow(r.Context(), service.TriggerRefreshNow, ctxdata.AdminID(r.Context()))
	reply(w, r, out, err)
}

func (a *admin) preview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope string `json:"scope,optional"`
		// ParamsOverride 可只给部分键：叠加在当前生效参数上（同 PUT /params 的 config 结构）
		ParamsOverride json.RawMessage `json:"paramsOverride,optional"`
	}
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	var override *algo.Config
	if len(req.ParamsOverride) > 0 && string(req.ParamsOverride) != "null" {
		cur, err := a.svc.Params(r.Context(), "")
		if err != nil {
			reply(w, r, nil, err)
			return
		}
		merged := cur.Current
		if err := json.Unmarshal(req.ParamsOverride, &merged); err != nil {
			reply(w, r, nil, xerr.Param("paramsOverride 非法："+err.Error()))
			return
		}
		override = &merged
	}
	out, err := a.svc.Preview(r.Context(), override, req.Scope)
	reply(w, r, out, err)
}

func (a *admin) params(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Params(r.Context(), r.URL.Query().Get("scope"))
	reply(w, r, v, err)
}

func (a *admin) saveParams(w http.ResponseWriter, r *http.Request) {
	var req service.SaveParamsReq
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	out, fields, err := a.svc.SaveParams(r.Context(), meta(r), req)
	if err != nil && len(fields) > 0 {
		code, msg, _ := xerr.Parse(err)
		httpx.OkJsonCtx(r.Context(), w, map[string]any{"code": code, "reason": service.ReasonParamsInvalid, "msg": msg, "data": map[string]any{"fields": fields}})
		return
	}
	reply(w, r, out, err)
}

func (a *admin) rules(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tpl, _ := strconv.ParseInt(q.Get("plazaTemplateId"), 10, 64)
	list, total, err := a.svc.ListRules(r.Context(), store.RuleFilter{Scope: q.Get("scope"), TemplateID: tpl, Phase: q.Get("phase"),
		Page: intQuery(r, "page", 1), Size: intQuery(r, "size", 20)})
	reply(w, r, page[service.RuleView]{Total: total, List: list}, err)
}

func (a *admin) createRule(w http.ResponseWriter, r *http.Request) {
	var req service.CreateRuleReq
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	out, err := a.svc.CreateRule(r.Context(), meta(r), req)
	reply(w, r, out, err)
}

func (a *admin) updateRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	var req service.UpdateRuleReq
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	out, err := a.svc.UpdateRule(r.Context(), meta(r), id, req)
	reply(w, r, out, err)
}

func (a *admin) deleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	out, err := a.svc.DeleteRule(r.Context(), meta(r), id, r.URL.Query().Get("applyToBatch") == "true")
	reply(w, r, out, err)
}

func (a *admin) position(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlazaTemplateID int64  `json:"plazaTemplateId"`
		Scope           string `json:"scope"`
		Position        *int   `json:"position,optional"`
	}
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	out, err := a.svc.Position(r.Context(), meta(r), req.PlazaTemplateID, req.Scope, req.Position)
	reply(w, r, out, err)
}

func (a *admin) why(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlazaTemplateID int64  `json:"plazaTemplateId"`
		Scope           string `json:"scope"`
		Why             string `json:"why"`
	}
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	out, err := a.svc.SetWhy(r.Context(), meta(r), req.PlazaTemplateID, req.Scope, req.Why)
	reply(w, r, out, err)
}

func (a *admin) suppress(w http.ResponseWriter, r *http.Request) {
	var req service.SuppressReq
	if err := decode(r, &req); err != nil {
		reply(w, r, nil, xerr.Param(err.Error()))
		return
	}
	out, err := a.svc.Suppress(r.Context(), meta(r), req)
	reply(w, r, out, err)
}

func (a *admin) suppressions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := a.svc.ListSuppressions(r.Context(), q.Get("scope"), q.Get("active") != "false")
	reply(w, r, out, err)
}

func (a *admin) unsuppress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	out, err := a.svc.Unsuppress(r.Context(), meta(r), id, r.URL.Query().Get("applyToBatch") == "true")
	reply(w, r, out, err)
}

// decode 标准库解码请求体（不走 go-zero 的字段映射：参数结构里有嵌套 map 与可空指针）；空体视为 {}。
func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}
