package plaza

import (
	"encoding/json"
	"net/http"
	"strconv"

	"aigc-local-life/app/plaza/core"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/v1wire"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest/pathvar"
)

// 用户的库：收藏、我的模板（需登录）与节点推荐（公开）。路由在 Register 里追加。

func pathID(r *http.Request, key string) (int64, error) {
	id, err := strconv.ParseInt(pathvar.Vars(r)[key], 10, 64)
	if err != nil || id <= 0 {
		return 0, xerr.Param(key + " 不合法")
	}
	return id, nil
}

func (h *handlers) addFavorite(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := pathID(r, "templateId")
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	out, err := s.AddFavorite(r.Context(), ctxdata.UserID(r.Context()), id)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) removeFavorite(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := pathID(r, "templateId")
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	out, err := s.RemoveFavorite(r.Context(), ctxdata.UserID(r.Context()), id)
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) listFavorites(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	out, err := s.ListFavorites(r.Context(), ctxdata.UserID(r.Context()), q.Get("industry"), q.Get("subIndustry"))
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, out)
}

// myTemplateBody 创建 / 修改的请求体；指针字段区分「没给」与「给了零值」。
type myTemplateBody struct {
	Name        *string                     `json:"name"`
	TemplateID  int64                       `json:"templateId"`
	Industry    string                      `json:"industry"`
	SubIndustry string                      `json:"subIndustry"`
	Inputs      *map[string]json.RawMessage `json:"inputs"`
	Variants    *int                        `json:"variants"`
	Resolution  *string                     `json:"resolution"`
	Text        *string                     `json:"text"`
}

func (b myTemplateBody) input() core.MyTemplateInput {
	return core.MyTemplateInput{Name: b.Name, TemplateID: b.TemplateID, Industry: b.Industry, SubIndustry: b.SubIndustry,
		Inputs: b.Inputs, Variants: b.Variants, Resolution: b.Resolution, Text: b.Text}
}

func (h *handlers) createMyTemplate(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	var body myTemplateBody
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	out, err := s.CreateMyTemplate(r.Context(), ctxdata.UserID(r.Context()), body.input())
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) listMyTemplates(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	out, err := s.ListMyTemplates(r.Context(), ctxdata.UserID(r.Context()), q.Get("industry"), q.Get("subIndustry"))
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	v1wire.OK(w, r, out)
}

func (h *handlers) updateMyTemplate(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	var body myTemplateBody
	if err := readJSON(r, &body); err != nil {
		v1wire.Fail(w, r, xerr.Param("请求体不是合法 JSON"))
		return
	}
	out, err := s.UpdateMyTemplate(r.Context(), ctxdata.UserID(r.Context()), id, body.input())
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, out)
}

func (h *handlers) deleteMyTemplate(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	if err := s.DeleteMyTemplate(r.Context(), ctxdata.UserID(r.Context()), id); err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.OK(w, r, map[string]any{"deleted": true})
}

func (h *handlers) nodes(w http.ResponseWriter, r *http.Request) {
	s := h.svc(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	out, err := s.Nodes(r.Context(), q.Get("industry"), q.Get("subIndustry"))
	if err != nil {
		v1wire.Fail(w, r, err)
		return
	}
	v1wire.Public(w, 60)
	v1wire.OK(w, r, out)
}
