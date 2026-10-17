// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/note"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 用户图文笔记列表（只读，排查用）
func ListHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.NoteListReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := note.NewListLogic(r.Context(), svcCtx)
		resp, err := l.List(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
