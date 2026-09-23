// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/videogen"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 规划剧本页的「重新生成」：重新构思
func ReplanVideoHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.IdPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := videogen.NewReplanVideoLogic(r.Context(), svcCtx)
		resp, err := l.ReplanVideo(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
