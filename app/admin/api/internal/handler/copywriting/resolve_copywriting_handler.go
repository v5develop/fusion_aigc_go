// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"net/http"

	"aigc-local-life/app/admin/api/internal/logic/copywriting"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 人工处理卡住的文案：仅限生成中且点数已扣（预留单已确认/部分退回）的记录；标记失败并记录处理说明，不自动退款，返回预留单状态供核对
func ResolveCopywritingHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ResolveCopywritingReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := copywriting.NewResolveCopywritingLogic(r.Context(), svcCtx)
		resp, err := l.ResolveCopywriting(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
