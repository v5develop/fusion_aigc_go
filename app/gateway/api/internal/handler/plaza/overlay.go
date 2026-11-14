package plaza

import (
	"net/http"

	"aigc-local-life/app/plaza/core"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/v1wire"
	"aigc-local-life/pkg/xerr"
)

// 模板文字叠加（DESIGN 17.3，P1）：出图后对自己的成品图叠字、改字 / 重试、按成品图恢复。均需登录；叠字不计费。

type overlayBody struct {
	TemplateID int64             `json:"templateId"`
	SourceURL  string            `json:"sourceUrl"`
	Values     map[string]string `json:"values"`
}

type overlayUpdateBody struct {
	Values *map[string]string `json:"values"`
}

func (h *handlers) createOverlay(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	var body overlayBody
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	out, err := s.CreateOverlay(r.Context(), ctxdata.UserID(r.Context()), body.TemplateID, body.SourceURL, body.Values)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, out)
}

func (h *handlers) updateOverlay(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	var body overlayUpdateBody
	if r.ContentLength != 0 { // 重试可以不带请求体
		if err := readJSON(r, &body); err != nil {
			v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
			return
		}
	}
	out, err := s.UpdateOverlay(r.Context(), ctxdata.UserID(r.Context()), id, body.Values)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, out)
}

func (h *handlers) getOverlay(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	out, err := s.GetOverlay(r.Context(), ctxdata.UserID(r.Context()), id)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, out)
}

func (h *handlers) listOverlays(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	items, err := s.ListOverlays(r.Context(), ctxdata.UserID(r.Context()), r.URL.Query()["sourceUrl"])
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, map[string]any{"items": items})
}

// createOverlayIntent 做同款提交前冻结本轮叠字意图（自动叠字）：{requestId(=本轮 X-Request-Id),templateId,enabled?,values}。
func (h *handlers) createOverlayIntent(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	var body core.OverlayIntentInput
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	out, err := s.CreateOverlayIntent(r.Context(), ctxdata.UserID(r.Context()), body)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, out)
}
