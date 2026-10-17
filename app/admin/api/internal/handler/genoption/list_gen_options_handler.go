// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package genoption

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/genoption"
	"aigc-local-life/app/admin/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 生成选项列表（模型 / 比例 / 时长，含停用项）
func ListGenOptionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := genoption.NewListGenOptionsLogic(r.Context(), svcCtx)
		resp, err := l.ListGenOptions()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
