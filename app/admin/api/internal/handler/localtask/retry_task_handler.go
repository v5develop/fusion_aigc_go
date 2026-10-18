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

// 运营重做生成失败的任务：origin_ref 形如 img:123（来源记录坐标）；按原内容重新冻结点数并重新提交
func RetryTaskHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.RetryTaskReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := localtask.NewRetryTaskLogic(r.Context(), svcCtx)
		resp, err := l.RetryTask(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
