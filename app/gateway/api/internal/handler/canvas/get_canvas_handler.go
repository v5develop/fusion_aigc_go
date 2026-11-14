// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"net/http"
	"strings"

	"aigc-local-life/app/gateway/api/internal/logic/canvas"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 取画布：快照 + revision，运行态按最近一次 run 回填；?revision= 取历史版本（最近 20 个）
func GetCanvasHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CanvasIdPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := canvas.NewGetCanvasLogic(r.Context(), svcCtx)
		// ?revision=：取历史某 revision 的快照（三方合并 base）；超出保留范围回 4001 · CANVAS_REVISION_NOT_RETAINED
		resp, err := l.GetCanvasAt(&req, strings.TrimSpace(r.URL.Query().Get("revision")))
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
