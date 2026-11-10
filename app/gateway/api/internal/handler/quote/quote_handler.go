// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package quote

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/quote"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 提交前报价（只读，不冻结）：按工具与当前表单参数，返回提交（或出片 / 出图）会冻结的点数、余额与是否足够；与提交走同一套计费请求构造与算价
func QuoteHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.LocalLifeQuoteReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := quote.NewQuoteLogic(r.Context(), svcCtx)
		resp, err := l.Quote(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
