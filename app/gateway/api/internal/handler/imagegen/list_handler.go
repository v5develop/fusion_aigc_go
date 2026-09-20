// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/imagegen"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 列表
func ListHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ListImageGenReq
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
