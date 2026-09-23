package middleware

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/result"
	"aigc-life/pkg/token"
	"aigc-life/pkg/xerr"
)

// AuthMiddleware 解析当前用户。
//
// 正常路径：Authorization: Bearer <我们签发的令牌>（登录接口用联盟 token 换来的）。
// Mode=dev 时，没带令牌还允许用请求头 X-User-Id 顶替，方便联调；线上必须设成 token。
type AuthMiddleware struct {
	mode string
	jwt  token.Conf
}

func NewAuthMiddleware(mode string, jwt token.Conf) *AuthMiddleware {
	return &AuthMiddleware{mode: mode, jwt: jwt}
}

func (m *AuthMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if raw := bearer(r); raw != "" {
			claims, err := token.Parse(m.jwt, raw)
			switch {
			case errors.Is(err, token.ErrExpired):
				result.Error(w, r, xerr.Unauthorized, "登录已过期，请重新登录")
				return
			case err != nil:
				result.Error(w, r, xerr.Unauthorized, "未登录")
				return
			}
			next(w, r.WithContext(ctxdata.WithUserID(r.Context(), claims.UserID)))
			return
		}
		if m.mode == "dev" { // 联调：信任 X-User-Id
			if uid, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64); err == nil && uid > 0 {
				next(w, r.WithContext(ctxdata.WithUserID(r.Context(), uid)))
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
