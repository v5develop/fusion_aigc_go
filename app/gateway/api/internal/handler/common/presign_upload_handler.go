// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/common"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 预签名上传地址（推荐）：后端签好 PUT 地址，前端直接 PUT 文件，无需密钥与签名
func PresignUploadHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.PresignUploadReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := common.NewPresignUploadLogic(r.Context(), svcCtx)
		resp, err := l.PresignUpload(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
