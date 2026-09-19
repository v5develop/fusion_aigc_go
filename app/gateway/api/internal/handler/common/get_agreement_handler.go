// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/common"
	"aigc-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// AIGC 功能使用约定：是否已同意当前版本
func GetAgreementHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := common.NewGetAgreementLogic(r.Context(), svcCtx)
		resp, err := l.GetAgreement()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
