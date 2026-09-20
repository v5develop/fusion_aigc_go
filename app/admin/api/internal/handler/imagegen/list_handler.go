// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"net/http"

	"aigc-life/app/admin/api/internal/logic/imagegen"
	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 用户海报/长图列表（只读，排查用）
func ListHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ImageGenListReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := imagegen.NewListLogic(r.Context(), svcCtx)
		resp, err := l.List(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
