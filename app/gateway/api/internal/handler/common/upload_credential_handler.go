// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/common"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 客户端直传 OSS 的临时凭证（限定只能写返回的 dir 目录）
func UploadCredentialHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.UploadCredentialReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := common.NewUploadCredentialLogic(r.Context(), svcCtx)
		resp, err := l.UploadCredential(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
