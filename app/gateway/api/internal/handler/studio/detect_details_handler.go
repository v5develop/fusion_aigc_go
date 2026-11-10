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

// 商拍「AI 找细节」（免费、不建任务）：对服装图找细节，返回每张图的细节框（归一化 [x,y,w,h]）与部位名；前端按框在原图上显示裁切缩略图，删减 / 重新裁切后随提交带回 detail_crops。按用户限频（超出 4002「识别太频繁，请稍后再试」）；上一次还没结束 1004
func DetectDetailsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioDetailsReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewDetectDetailsLogic(r.Context(), svcCtx)
		resp, err := l.DetectDetails(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
