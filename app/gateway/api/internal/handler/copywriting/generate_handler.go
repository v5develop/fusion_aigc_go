// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/copywriting"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// AI帮写或爆款仿写：提交生成
func GenerateHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.GenerateReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := copywriting.NewGenerateLogic(r.Context(), svcCtx)
		resp, err := l.Generate(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
