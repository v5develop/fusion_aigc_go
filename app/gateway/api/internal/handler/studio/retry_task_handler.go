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

// 重试单个失败子任务（仅本人、任务失败且批次为全部失败 / 部分成功）：只重跑这张图的生图一步（沿用已存提示词，不再反推），按原入口重新冻结 1 张点数；返回批次详情（同 GET /batches/:id），之后照常轮询批次。重复点击幂等
func RetryTaskHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioTaskPath
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewRetryTaskLogic(r.Context(), svcCtx)
		resp, err := l.RetryTask(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
