// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/digitalhuman"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 数字人选项列表（年龄/肤色/职业/音色分类/提示词）
func ListOptionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.DhOptionListReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := digitalhuman.NewListOptionsLogic(r.Context(), svcCtx)
		resp, err := l.ListOptions(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
