// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"net/http"

	"aigc-local-life/app/gateway/api/internal/logic/studio"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 姿态复刻多人配对预览（只读、不计费）：返回原图与每张姿势图的人数、人物站位与默认配对；原图 ≥2 人时前端据此让用户确认 / 反转配对，再把 mappings 随提交带回
func PreviewPoseHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.StudioPosePreviewReq
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := studio.NewPreviewPoseLogic(r.Context(), svcCtx)
		resp, err := l.PreviewPose(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
