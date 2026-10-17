// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/videogen"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 确认剧本，开始生成视频（探店 73 / 短剧 88 / 视频复刻 71）
func GenerateVideoHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.GenerateVideoReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := videogen.NewGenerateVideoLogic(r.Context(), svcCtx)
		resp, err := l.GenerateVideo(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
