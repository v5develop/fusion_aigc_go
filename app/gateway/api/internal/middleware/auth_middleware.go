package middleware

import (
	"net/http"
	"strconv"

	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/result"
	"aigc-life/pkg/xerr"
)

// AuthMiddleware 解析当前用户。
// TODO 待接入现有用户体系（afv_app_user）：需要确认 token 怎么验（JWT 密钥或调用某个服务）。
// 当前为 dev 模式：信任请求头 X-User-Id，仅限开发环境，上线前必须替换。
type AuthMiddleware struct {
	mode string
}

func NewAuthMiddleware(mode string) *AuthMiddleware {
	return &AuthMiddleware{mode: mode}
}

func (m *AuthMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64)
		if err != nil || uid <= 0 {
			result.Error(w, r, xerr.Unauthorized, "未登录")
			return
		}
		next(w, r.WithContext(ctxdata.WithUserID(r.Context(), uid)))
	}
}
