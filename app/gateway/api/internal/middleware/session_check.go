package middleware

import (
	"net/http"
	"strconv"

	"aigc-local-life/pkg/xerr"
)

// SessionValid 复核请求的登录态是否仍有效且仍是 uid（SSE 等长连接在心跳时用；与 Handle 同一套解析：共用会话 / JWT / dev 的 X-User-Id）。
// ok=false：已失效（过期、被踢下线、换了用户、没有凭证）；transient=true：查询失败（如 Redis 抖动），调用方不应据此断开。
func (m *AuthMiddleware) SessionValid(r *http.Request, uid int64) (ok, transient bool) {
	if raw := bearer(r); raw != "" {
		got, code, _ := m.resolve(r, raw)
		switch {
		case code == xerr.ServerError:
			return false, true
		case code != 0:
			return false, false
		}
		return got == uid, false
	}
	if m.mode == "dev" {
		got, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64)
		return err == nil && got == uid, false
	}
	return false, false
}
