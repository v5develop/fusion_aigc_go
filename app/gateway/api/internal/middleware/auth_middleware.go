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

	"github.com/zeromicro/go-zero/core/logx"
)

// AuthMiddleware 解析当前用户。
//
// 正常路径：Authorization: Bearer <令牌>，由 /auth/login 用联盟的**临时 token** 换来。
// 临时 token 一次性（5 分钟、用一次即失效），只能换一次，不能当每次请求的凭证。
//
// 令牌有两种，按形状自动区分：
//   - 32 位 hex：和 AIGC（Java）服务同一套的会话令牌，身份在共用的 Redis 里 —— 一个 token 两边都认；
//   - 三段点分：我们自己签的 JWT（没配 Session 时的回落）。
//
// Mode=dev 时，没带令牌还允许用请求头 X-User-Id 顶替，方便联调；线上必须设成 token。
type AuthMiddleware struct {
	mode string
	jwt  token.Conf
	sess *session.Store
}

func NewAuthMiddleware(mode string, jwt token.Conf, sess *session.Store) *AuthMiddleware {
	return &AuthMiddleware{mode: mode, jwt: jwt, sess: sess}
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

// resolve 认出令牌背后的用户。
func (m *AuthMiddleware) resolve(r *http.Request, raw string) (uid int64, code int, msg string) {
	// 和 AIGC 服务共用的会话令牌：32 位 hex，身份在 Redis
	if m.sess.Enabled() && session.Looks(raw) {
		c, err := m.sess.Parse(r.Context(), raw)
		switch {
		case errors.Is(err, session.ErrInvalid):
			return 0, xerr.Unauthorized, "登录已过期，请重新登录"
		case err != nil:
			// Redis 抖动不是前端的错，别让它跳登录页
			logx.WithContext(r.Context()).Errorf("parse session: %v", err)
			return 0, xerr.ServerError, "登录状态查询失败，请稍后重试"
		}
		return c.UserID, 0, ""
	}
	// 会话开启（正式环境）：令牌只认共用会话，不再解析 JWT，免得一个不该存在的签名路径留着
	if m.sess.Enabled() {
		return 0, xerr.Unauthorized, "未登录"
	}
	claims, err := token.Parse(m.jwt, raw)
	switch {
	case errors.Is(err, token.ErrExpired):
		return 0, xerr.Unauthorized, "登录已过期，请重新登录"
	case err != nil:
		return 0, xerr.Unauthorized, "未登录"
	}
	return claims.UserID, 0, ""
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
