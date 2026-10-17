// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package shopprofile

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/shopprofile"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 创建店铺档案
func CreateShopProfileHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ShopProfileForm
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := shopprofile.NewCreateShopProfileLogic(r.Context(), svcCtx)
		resp, err := l.CreateShopProfile(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
