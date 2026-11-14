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

// 我的标签字典（含每个标签的资产数；默认按使用次数倒序，sort=name 按名字）
func ListTagsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.AssetLibTagListReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := assetlib.NewListTagsLogic(r.Context(), svcCtx)
		resp, err := l.ListTags(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
