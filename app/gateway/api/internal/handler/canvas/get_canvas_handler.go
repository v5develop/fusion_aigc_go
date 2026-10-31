// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/canvas"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 取画布：快照 + revision，运行态按最近一次 run 回填
func GetCanvasHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CanvasIdPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := canvas.NewGetCanvasLogic(r.Context(), svcCtx)
		resp, err := l.GetCanvas(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
