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

// 新增我的提示词（每人 ≤500 条）
func CreatePromptHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.PromptLibUpsertReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := promptlib.NewCreatePromptLogic(r.Context(), svcCtx)
		resp, err := l.CreatePrompt(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
