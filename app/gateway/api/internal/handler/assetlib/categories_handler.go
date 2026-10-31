// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/assetlib"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 分类条计数：all + 5 个 kind + uncategorized，随 source / media
func CategoriesHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.AssetLibCategoriesReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := assetlib.NewCategoriesLogic(r.Context(), svcCtx)
		resp, err := l.Categories(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
