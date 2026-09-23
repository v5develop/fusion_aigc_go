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

// 提交表单，开始构思剧本（探店 72 / 短剧 87）
func CreateVideoHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CreateVideoReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := videogen.NewCreateVideoLogic(r.Context(), svcCtx)
		resp, err := l.CreateVideo(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
