package canvas

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/canvas"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 生图预估点数（只读，不冻结）：与执行时冻结同一个 Quote
func EstimateHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CanvasEstimateReq
		if err := parseRaw(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := canvas.NewEstimateLogic(r.Context(), svcCtx)
		resp, err := l.Estimate(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
