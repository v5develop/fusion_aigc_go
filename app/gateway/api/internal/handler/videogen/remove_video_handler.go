// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/videogen"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 删除
func RemoveVideoHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.IdPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := videogen.NewRemoveVideoLogic(r.Context(), svcCtx)
		resp, err := l.RemoveVideo(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
