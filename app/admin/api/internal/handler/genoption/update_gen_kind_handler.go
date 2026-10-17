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

// 改生成类型的模板必选开关（海报 / 长图 / 动作迁移）
func UpdateGenKindHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.UpdateGenKindReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := genoption.NewUpdateGenKindLogic(r.Context(), svcCtx)
		resp, err := l.UpdateGenKind(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
