// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/digitalhuman"
	"aigc-local-life/app/admin/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 当前配音模型（只读）
func GetTTSModelHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := digitalhuman.NewGetTTSModelLogic(r.Context(), svcCtx)
		resp, err := l.GetTTSModel()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
