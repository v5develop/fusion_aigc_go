package canvas

import (
	"errors"
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/canvas"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 保存（乐观锁 if_match_revision，冲突 code=4002 data.conflict=revision；force=true 覆盖）
func SaveCanvasHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.CanvasSaveReq
		if err := parseSave(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := canvas.NewSaveCanvasLogic(r.Context(), svcCtx)
		resp, err := l.SaveCanvas(&req)
		var ce *canvas.ConflictError
		switch {
		case errors.As(err, &ce):
			// 版本冲突：信封里带服务端当前版本，前端按 data.conflict 区分「载入最新版本」/「覆盖保存」
			httpx.WriteJsonCtx(r.Context(), w, http.StatusOK, result.Body{
				Code: xerr.Conflict, Reason: xerr.Reason(xerr.Conflict), Msg: ce.Error(),
				Data: map[string]any{"conflict": "revision", "revision": ce.Revision, "updated_at": ce.UpdatedAt},
			})
		case err != nil:
			httpx.ErrorCtx(r.Context(), w, err)
		default:
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
