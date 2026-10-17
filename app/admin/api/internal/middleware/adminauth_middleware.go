package middleware

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/token"
	"aigc-local-life/pkg/xerr"
)

// AdminAuthMiddleware 解析当前管理员。
//
// 正常路径：Authorization: Bearer <令牌>，由 /admin/v1/auth/login 用现有后台账号
// （afv_admin_user，密码是 BCrypt）换来。令牌密钥与 App 那套分开，两边不通用。
//
// Mode=dev 时，没带令牌还允许用请求头 X-Admin-Id 顶替，方便联调；线上必须设成 token。
type AdminAuthMiddleware struct {
	mode string
	jwt  token.Conf
}

func NewAdminAuthMiddleware(mode string, jwt token.Conf) *AdminAuthMiddleware {
	return &AdminAuthMiddleware{mode: mode, jwt: jwt}
}

func (m *AdminAuthMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if raw := bearer(r); raw != "" {
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
