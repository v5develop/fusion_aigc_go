// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/imagegen"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 模板列表（海报可按类型筛）
func TemplatesHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ImageGenTemplatesReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := imagegen.NewTemplatesLogic(r.Context(), svcCtx)
		resp, err := l.Templates(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
