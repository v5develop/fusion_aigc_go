package middleware

import (
	"errors"
	"net/http"
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
	code := permUpdate
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		code = permView
	}
	ok, err := session.AllowAdmin(r.Context(), m.conn, a.AdminID, code)
	if err != nil {
		result.Error(w, r, xerr.ServerError, "权限校验失败")
		return
	}
	if !ok {
		result.Error(w, r, xerr.Forbidden, "没有权限："+code)
		return
	}
	next(w, r.WithContext(ctxdata.WithAdminID(r.Context(), a.AdminID)))
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
