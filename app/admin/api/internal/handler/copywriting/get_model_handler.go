// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/copywriting"
	"aigc-local-life/app/admin/api/internal/svc"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 当前写作模型（只读）
func GetModelHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := copywriting.NewGetModelLogic(r.Context(), svcCtx)
		resp, err := l.GetModel()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
