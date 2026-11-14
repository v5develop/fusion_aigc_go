package middleware

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/token"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 本地生活后台的权限码（与 Java 的 RBAC 同一套表，见迁移 Go_V1.0.14）。
const (
	permView   = "local_life:view"
	permUpdate = "local_life:update"
)

// prefixPerms 按路由前缀单独取权限码的模块；没列出的前缀用本地生活的码。
var prefixPerms = []struct{ prefix, view, update string }{
	{"/api/v1/asset-library/admin", "asset_library:view", "asset_library:update"}, // Go_V1.0.24
	{"/api/v1/studio/admin", "studio:view", "studio:update"},                      // Go_V1.0.22（GET 列表/详情/依赖/成本 = view；POST 重做/强制失败 = update）
	{"/api/v1/canvas/admin", "canvas:view", "canvas:update"},                      // Go_V1.0.26（GET = view；POST/PUT 强制失败、模板 / 提示词维护 = update）
	{"/api/v1/prompt-library/admin", "canvas:view", "canvas:update"},              // Go_V1.0.26（提示词库与画布共用一组权限）
	{"/api/v1/plaza/admin", "creaft_catalog:view", "creaft_catalog:edit"},         // 复用 Java 后台已有权限码（角色已授权）；高风险路由见 routePerm
	{"/api/v1/replicate/admin", "replicate:view", "replicate:force"},              // Go_V1.0.39（GET = view；退点 / 收尾见 replicateRoutePerm）
}

const plazaAdminPrefix = "/api/v1/plaza/admin/"

// permIndustryRelation 改模板行业关系的路由（PUT /templates/{id}/industries、POST /templates/batch/{assign|unassign|replaceIndustries|setOrder}）
// 在中间件这一层要编辑权限；对已发布或已有行业的模板，改关系还要 plaza:publish——这取决于模板数据，
// 由 handler 查 Allowed("plaza:publish") 填 core.Operator.CanPublish、core 在事务内按模板状态判定（未发布且未归类的草稿不受影响）。
const permIndustryRelation = "creaft_catalog:edit"

// IndustryRelationAction 会改变模板行业关系的批量动作（handler 据此为请求补查发布权限）。
func IndustryRelationAction(action string) bool {
	switch action {
	case "assign", "unassign", "replaceIndustries", "setOrder":
		return true
	}
	return false
}

// routePerm 广场后台的路由级权限覆盖（Go_V1.0.36）：发布 / 下线、删除 / 恢复 / 彻底删除、审计查看比普通读写风险高。
// 先于前缀映射匹配；没命中返回 ""。p 必须是 path.Clean 之后的路径（批量按动作拆成独立路由，所以不用读 body）。
func routePerm(method, p string) string {
	if code := replicateRoutePerm(method, p); code != "" {
		return code
	}
	if !strings.HasPrefix(p, plazaAdminPrefix) {
		return ""
	}
	seg := strings.Split(strings.TrimPrefix(p, plazaAdminPrefix), "/")
	switch {
	case seg[0] == "audit" && len(seg) == 1 && method == http.MethodGet:
		return "plaza:audit"
	case seg[0] != "templates":
		return ""
	}
	switch {
	case len(seg) == 3 && seg[1] == "batch" && method == http.MethodPost: // /templates/batch/{action}
		switch seg[2] {
		case "publish", "offline":
			return "plaza:publish"
		case "delete", "restore":
			return "plaza:delete"
		}
		if IndustryRelationAction(seg[2]) {
			return permIndustryRelation
		}
	case len(seg) == 3 && method == http.MethodPut && seg[1] != "batch" && seg[2] == "industries": // PUT /templates/{id}/industries
		return permIndustryRelation
	case len(seg) == 3 && method == http.MethodGet && seg[2] == "audit": // /templates/{id}/audit
		return "plaza:audit"
	case len(seg) == 3 && method == http.MethodPost && (seg[2] == "publish" || seg[2] == "offline"):
		return "plaza:publish"
	case len(seg) == 3 && method == http.MethodPost && seg[2] == "restore":
		return "plaza:delete"
	case len(seg) == 3 && method == http.MethodDelete && seg[2] == "purge":
		return "plaza:delete"
	case len(seg) == 2 && method == http.MethodDelete && seg[1] != "batch": // DELETE /templates/{id}
		return "plaza:delete"
	}
	return ""
}

// permFor 当前请求需要的权限码：GET / HEAD 要 view，其它要 update。
// 先 path.Clean 再匹配（go-zero 路由按清理后的路径分发；用原始路径会被 `//`、`..` 绕到别的模块的权限上），
// 前缀按整段匹配：等于前缀或以「前缀/」开头。
func permFor(r *http.Request) string {
	view, update := permView, permUpdate
	p := cleanPath(r.URL.Path)
	if code := routePerm(r.Method, p); code != "" {
		return code
	}
	for _, x := range prefixPerms {
		if p == x.prefix || strings.HasPrefix(p, x.prefix+"/") {
			view, update = x.view, x.update
			break
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return view
	}
	return update
}

// cleanPath 与路由同口径的路径清理：path.Clean，并保证以 / 开头。
func cleanPath(p string) string {
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return path.Clean(p)
}

// permCheckerKey 请求 ctx 里的权限判断函数（共享模式下由中间件放入）。
type permCheckerKey struct{}

// WithPermChecker 把权限判断函数放进 ctx（中间件共享模式下调用；单测也用它模拟）。
func WithPermChecker(ctx context.Context, fn func(context.Context, string) (bool, error)) context.Context {
	return context.WithValue(ctx, permCheckerKey{}, fn)
}

// Allowed 当前管理员是否有某权限码：与中间件同一套判断（session.AllowAdmin，Java RBAC 表）。
// 独立模式（没配会话 Redis，仅开发）没有 RBAC，返回 true，与中间件放行口径一致。
// 给 handler / logic 做「同一接口按数据再细分权限」用，如本地生活任务入口里的 studio 任务要 studio:update。
func Allowed(ctx context.Context, code string) (bool, error) {
	fn, ok := ctx.Value(permCheckerKey{}).(func(context.Context, string) (bool, error))
	if !ok {
		return true, nil
	}
	return fn(ctx, code)
}

// Require Allowed 的报错版：没有权限返回 Forbidden（文案同中间件）。
func Require(ctx context.Context, code string) error {
	ok, err := Allowed(ctx, code)
	if err != nil {
		return xerr.New(xerr.ServerError, "权限校验失败")
	}
	if !ok {
		return xerr.New(xerr.Forbidden, "没有权限："+code)
	}
	return nil
}

// AdminAuthMiddleware 解析当前管理员。
//
// 共享模式（配了 Auth.Session，正式环境）：令牌就是 Java 后台登录发的那一个，存在同一个 Redis 里。
// 每个请求按 Java 的 RBAC 查权限：GET 要 local_life:view，其它要 local_life:update。
//
// 独立模式（没配会话 Redis，仅开发）：用本服务自己签的 JWT；Mode=dev 时还可用 X-Admin-Id 顶替。
type AdminAuthMiddleware struct {
	mode string
	jwt  token.Conf
	sess *session.Store
	conn sqlx.SqlConn
}

func NewAdminAuthMiddleware(mode string, jwt token.Conf, sess *session.Store, conn sqlx.SqlConn) *AdminAuthMiddleware {
	return &AdminAuthMiddleware{mode: mode, jwt: jwt, sess: sess, conn: conn}
}

func (m *AdminAuthMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if m.sess.Enabled() {
			m.handleShared(w, r, raw, next)
			return
		}
		if raw != "" {
			claims, err := token.ParseAdmin(m.jwt, raw)
			switch {
			case errors.Is(err, token.ErrExpired):
				result.Error(w, r, xerr.Unauthorized, "登录已过期，请重新登录")
				return
			case err != nil:
				result.Error(w, r, xerr.Unauthorized, "未登录")
				return
			}
			next(w, r.WithContext(ctxdata.WithAdminID(r.Context(), claims.UserID)))
			return
		}
		if m.mode == "dev" { // 联调：信任 X-Admin-Id
			if id, err := strconv.ParseInt(r.Header.Get("X-Admin-Id"), 10, 64); err == nil && id > 0 {
				next(w, r.WithContext(ctxdata.WithAdminID(r.Context(), id)))
				return
			}
		}
		result.Error(w, r, xerr.Unauthorized, "未登录")
	}
}

func (m *AdminAuthMiddleware) handleShared(w http.ResponseWriter, r *http.Request, raw string, next http.HandlerFunc) {
	if raw == "" || !session.Looks(raw) {
		result.Error(w, r, xerr.Unauthorized, "未登录")
		return
	}
	a, err := m.sess.ParseAdmin(r.Context(), raw)
	if errors.Is(err, session.ErrInvalid) {
		result.Error(w, r, xerr.Unauthorized, "登录已过期，请重新登录")
		return
	}
	if err != nil {
		result.Error(w, r, xerr.ServerError, "后台会话校验失败")
		return
	}
	allow := func(ctx context.Context, code string) (bool, error) {
		return session.AllowAdmin(ctx, m.conn, a.AdminID, code)
	}
	code := permFor(r)
	ok, err := allow(r.Context(), code)
	if err != nil {
		result.Error(w, r, xerr.ServerError, "权限校验失败")
		return
	}
	if !ok {
		result.Error(w, r, xerr.Forbidden, "没有权限："+code)
		return
	}
	ctx := WithPermChecker(ctxdata.WithAdminID(r.Context(), a.AdminID), allow)
	next(w, r.WithContext(ctx))
}

// bearer 取 Authorization 头里的令牌。
func bearer(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if v == "" {
		return ""
	}
	if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

const replicateAdminPrefix = "/api/v1/replicate/admin/"

// replicateRoutePerm 视频做同款后台的路由级权限（Go_V1.0.39）：POST /jobs/{id}/refund 要 replicate:refund，
// POST /jobs/{id}/force-finish 要 replicate:force；示例管理 /samples/** 的写（POST / PUT / DELETE，Go_V1.0.41）要 replicate:sample；
// 其余走前缀映射（GET = replicate:view，其它写 = replicate:force，从严）。
func replicateRoutePerm(method, p string) string {
	if !strings.HasPrefix(p, replicateAdminPrefix) {
		return ""
	}
	seg := strings.Split(strings.TrimPrefix(p, replicateAdminPrefix), "/")
	if seg[0] == "samples" && method != http.MethodGet && method != http.MethodHead {
		return "replicate:sample"
	}
	if method != http.MethodPost {
		return ""
	}
	if len(seg) != 3 || seg[0] != "jobs" {
		return ""
	}
	switch seg[2] {
	case "refund":
		return "replicate:refund"
	case "force-finish":
		return "replicate:force"
	}
	return ""
}
