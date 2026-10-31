// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/assetlib"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 分类统计：按 kind × 来源计数
func KindStatsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.AssetLibKindStatsReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := assetlib.NewKindStatsLogic(r.Context(), svcCtx)
		resp, err := l.KindStats(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
