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

// 选中候选：写回 run.selected_index，刷新后 selectedCandidate 仍是它
func SelectCandidateHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CanvasSelectCandidateReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := canvas.NewSelectCandidateLogic(r.Context(), svcCtx)
		resp, err := l.SelectCandidate(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
