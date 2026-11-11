// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/studio"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 重新存入素材库：付费成品转存素材库永久失败（library_status=failed）时重新转存；仅本人；已在转存中 / 已在素材库时幂等返回当前状态；同一子任务每分钟至多一次（否则 4002）；返回该任务（同 GET /tasks/:task_id，library_status 变为 pending），之后照常轮询
func RetryLibraryCopyHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioTaskPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewRetryLibraryCopyLogic(r.Context(), svcCtx)
		resp, err := l.RetryLibraryCopy(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
