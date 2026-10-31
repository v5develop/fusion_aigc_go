// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/studio"
	"aigc-local-life/app/admin/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 依赖状态：反推 sidecar / ai-gateway / 媒体服务
func SidecarStatusHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := studio.NewSidecarStatusLogic(r.Context(), svcCtx)
		resp, err := l.SidecarStatus()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
