package middleware

import (
	"net/http"
	"strconv"

	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/result"
	"aigc-life/pkg/xerr"
)

// AdminAuthMiddleware 解析当前管理员。
// TODO 待接入现有 Admin 体系（afv_admin_user / afv_admin_permission）：需要确认登录态与权限校验方式。
// 当前为 dev 模式：信任请求头 X-Admin-Id，仅限开发环境，上线前必须替换。
type AdminAuthMiddleware struct {
	mode string
}

func NewAdminAuthMiddleware(mode string) *AdminAuthMiddleware {
	return &AdminAuthMiddleware{mode: mode}
}

func (m *AdminAuthMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.Header.Get("X-Admin-Id"), 10, 64)
		if err != nil || id <= 0 {
			result.Error(w, r, xerr.Unauthorized, "未登录")
			return
		}
		next(w, r.WithContext(ctxdata.WithAdminID(r.Context(), id)))
	}
}
