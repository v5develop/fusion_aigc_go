// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/assetlib"
	"aigc-local-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 系统标签字典：本人资产里出现过的系统标签及资产数（按数量倒序，最多 500 个）
func ListSysTagsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := assetlib.NewListSysTagsLogic(r.Context(), svcCtx)
		resp, err := l.ListSysTags()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
