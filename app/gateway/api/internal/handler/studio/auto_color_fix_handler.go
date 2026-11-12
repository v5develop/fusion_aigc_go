// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/studio"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 参考图驱动的自动校色（免费，只增 2026-11-12）：按商拍换装子任务（本人、成功）取成品、各单品的服装参考图与逐件蒙版，交给求解服务返回每件单品的五个校色滑杆值（作为该单品图层的初值，不落库，用户点「应用」才保存）；按用户限频（超出 4002 · COLORFIX_AUTO_TOO_FREQUENT）；功能没开 7003 · COLORFIX_AUTO_DISABLED；服务忙 / 超时 6001 · COLORFIX_AUTO_BUSY / COLORFIX_AUTO_TIMEOUT（可重试）
func AutoColorFixHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioAutoColorFixReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewAutoColorFixLogic(r.Context(), svcCtx)
		resp, err := l.AutoColorFix(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
