// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 公共数字人列表
func ListPublicHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := digitalhuman.NewListPublicLogic(r.Context(), svcCtx)
		resp, err := l.ListPublic()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
