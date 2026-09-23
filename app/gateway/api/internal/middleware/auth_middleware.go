package middleware

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/result"
	"aigc-life/pkg/token"
	"aigc-life/pkg/unionauth"
	"aigc-life/pkg/xerr"
)

// AuthMiddleware 解析当前用户。Authorization: Bearer <凭证>，凭证可以是：
//
//   - 联盟（xxlm）的 token：前端登录联盟后本来就持有，直接拿来调我们的接口即可，
//     我们换成用户信息并缓存一段时间（见 UnionSession），不用再管第二个令牌；
//   - 我们自己签发的令牌：/auth/login 返回的那个，联盟服务不可用时的后路。
//
// Mode=dev 时，没带凭证还允许用请求头 X-User-Id 顶替，方便联调；线上必须设成 token。
type AuthMiddleware struct {
	mode    string
	jwt     token.Conf
	session *UnionSession
}

func NewAuthMiddleware(mode string, jwt token.Conf, session *UnionSession) *AuthMiddleware {
	return &AuthMiddleware{mode: mode, jwt: jwt, session: session}
}

func (m *AuthMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if raw := bearer(r); raw != "" {
			uid, code, msg := m.resolve(r, raw)
			if code != 0 {
				result.Error(w, r, code, msg)
				return
			}
			next(w, r.WithContext(ctxdata.WithUserID(r.Context(), uid)))
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

// resolve 认出凭证背后的用户；失败时返回要回给前端的错误码与提示。
func (m *AuthMiddleware) resolve(r *http.Request, raw string) (uid int64, code int, msg string) {
	if ours(raw) {
		claims, err := token.Parse(m.jwt, raw)
		switch {
		case errors.Is(err, token.ErrExpired):
			return 0, xerr.Unauthorized, "登录已过期，请重新登录"
		case err != nil:
			return 0, xerr.Unauthorized, "未登录"
		}
		return claims.UserID, 0, ""
	}
	if !m.session.Enabled() {
		return 0, xerr.Unauthorized, "未登录"
	}
	id, err := m.session.Resolve(r.Context(), raw)
	switch {
	case errors.Is(err, unionauth.ErrUnauthorized):
		return 0, xerr.Unauthorized, "登录已失效，请重新登录"
	case errors.Is(err, ErrDisabled):
		return 0, xerr.Forbidden, "账号已被禁用"
	case err != nil:
		// 联盟服务抖动/超时：不是前端的错，给个能重试的提示
		return 0, xerr.ServerError, "登录服务暂时不可用，请稍后重试"
	}
	return id, 0, ""
}

// ours 判断是不是我们签发的令牌：JWT 是三段点分的，联盟 token 是一串不带点的。
func ours(raw string) bool { return strings.Count(raw, ".") == 2 }

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
