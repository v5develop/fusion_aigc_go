// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/canvas"
	"aigc-local-life/app/gateway/api/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 模型 / 参数 / 工具目录（替代前端写死）
func CatalogHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := canvas.NewCatalogLogic(r.Context(), svcCtx)
		resp, err := l.Catalog()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
