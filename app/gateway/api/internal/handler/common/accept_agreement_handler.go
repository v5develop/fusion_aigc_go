// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/common"
	"aigc-local-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 同意 AIGC 功能使用约定
func AcceptAgreementHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := common.NewAcceptAgreementLogic(r.Context(), svcCtx)
		resp, err := l.AcceptAgreement()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
