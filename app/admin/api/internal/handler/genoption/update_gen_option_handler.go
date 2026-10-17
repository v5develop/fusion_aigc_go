// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package genoption

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/genoption"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 改生成选项：名称 / 启用 / 排序 / 默认（编码不可改）
func UpdateGenOptionHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.UpdateGenOptionReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := genoption.NewUpdateGenOptionLogic(r.Context(), svcCtx)
		resp, err := l.UpdateGenOption(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
