// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package localtask

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/localtask"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 运营强制失败生成失败的任务：origin_ref 形如 img:123；封禁运营重做，点数已退回不动账
func ForceFailTaskHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ForceFailTaskReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := localtask.NewForceFailTaskLogic(r.Context(), svcCtx)
		resp, err := l.ForceFailTask(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
