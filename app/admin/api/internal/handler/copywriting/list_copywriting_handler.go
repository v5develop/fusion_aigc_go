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

// 用户文案列表
func ListCopywritingHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CopywritingListReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := copywriting.NewListCopywritingLogic(r.Context(), svcCtx)
		resp, err := l.ListCopywriting(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
