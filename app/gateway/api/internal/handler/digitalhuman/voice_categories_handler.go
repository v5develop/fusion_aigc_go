// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/digitalhuman"
	"aigc-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 系统音色分类
func VoiceCategoriesHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := digitalhuman.NewVoiceCategoriesLogic(r.Context(), svcCtx)
		resp, err := l.VoiceCategories()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
