// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/note"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 确认开始生成；生成失败后再次调用即重新生成
func GenerateHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.GenerateNoteReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := note.NewGenerateLogic(r.Context(), svcCtx)
		resp, err := l.Generate(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
