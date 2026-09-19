// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/note"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 开始构思笔记（异步，轮询详情；完成后进入规划页）
func PlanHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.PlanNoteReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := note.NewPlanLogic(r.Context(), svcCtx)
		resp, err := l.Plan(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
