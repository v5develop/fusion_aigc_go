// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/common"
	"aigc-life/app/gateway/api/internal/svc"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 上传照片（multipart 字段 file），返回公网地址
func UploadHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := common.NewUploadLogic(r.Context(), svcCtx)
		resp, err := l.Upload(r)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
