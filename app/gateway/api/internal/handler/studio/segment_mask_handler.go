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

// 区域蒙版（局部校色，免费）：识别图片的服装 / 商品 / 人物 / 背景区域，返回白色+软 alpha 的 PNG 蒙版；按（去查询串的地址, 区域）缓存，重复请求立即返回。按用户限频（超出 4002「识别太频繁，请稍后再试」）；没识别到 4001「没识别到对应区域」；识别服务不可用 6001
func SegmentMaskHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioMaskReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewSegmentMaskLogic(r.Context(), svcCtx)
		resp, err := l.SegmentMask(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
