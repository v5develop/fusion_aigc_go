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

// 商拍换装提交前报价（只读，不冻结）：请求体同提交，返回本次提交预计冻结的点数
func QuoteTryonHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioTryonQuoteReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewQuoteTryonLogic(r.Context(), svcCtx)
		resp, err := l.QuoteTryon(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
