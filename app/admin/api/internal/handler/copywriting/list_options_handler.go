// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"net/http"

	"aigc-life/app/admin/api/internal/logic/copywriting"
	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 选项列表
func ListOptionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.OptionListReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := copywriting.NewListOptionsLogic(r.Context(), svcCtx)
		resp, err := l.ListOptions(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
