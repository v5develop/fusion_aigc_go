// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/copywriting"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 删除选项
func DeleteOptionHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.IdPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := copywriting.NewDeleteOptionLogic(r.Context(), svcCtx)
		resp, err := l.DeleteOption(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
