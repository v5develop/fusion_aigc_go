// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"net/http"

	"aigc-life/app/gateway/api/internal/logic/copywriting"
	"aigc-life/app/gateway/api/internal/svc"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 文案选项：内容类型/风格/行业/字数 + 当前模型
func OptionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := copywriting.NewOptionsLogic(r.Context(), svcCtx)
		resp, err := l.Options()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
