// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package promptlib

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/promptlib"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 修改我的提示词（官方 4002）
func UpdatePromptHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.PromptLibUpdateReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := promptlib.NewUpdatePromptLogic(r.Context(), svcCtx)
		resp, err := l.UpdatePrompt(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
