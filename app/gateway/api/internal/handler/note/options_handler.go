// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/note"
	"aigc-local-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 图文笔记表单配置：笔记类型、素材与套图上限
func OptionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := note.NewOptionsLogic(r.Context(), svcCtx)
		resp, err := l.Options()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
