// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/assetlib"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 批量删除资产（逻辑删除 + 解除标签）：只删本人的上传 / 生成资产，官方素材与不存在的静默跳过并计数；不删存储对象、不影响任务结果
func BatchDeleteAssetsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.AssetLibBatchDeleteReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := assetlib.NewBatchDeleteAssetsLogic(r.Context(), svcCtx)
		resp, err := l.BatchDeleteAssets(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
