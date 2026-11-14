package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/xerr"
)

// 示例 / 改标注 / 我的方案（docs/api/api-replicate.md §10）。限频口径：改标注、再做一条走独立计数 replicate.rate.annotate_*
// （不占「方案」额度；失败 / 重放退回）；
// 示例列表、试看、取方案、保存 / 列表 / 删除不计。

// samples GET /samples?industry=：已启用且已有分析的示例。
func (h *handlers) samples(_ http.ResponseWriter, r *http.Request, s *core.Service, _ int64) (any, error) {
	industry := strings.TrimSpace(r.URL.Query().Get("industry"))
	if len(industry) > 64 {
		return nil, xerr.Param("industry 不合法")
	}
	return s.ListSamples(r.Context(), industry)
}

// trySample POST /samples/{id}/try：共享分析 + 示例成片，零成本（不建 job、不预留、不计限频）。
func (h *handlers) trySample(_ http.ResponseWriter, r *http.Request, s *core.Service, _ int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return s.TrySample(r.Context(), id)
}

func (h *handlers) getPlan(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return s.GetPlan(r.Context(), uid, no)
}

// planEdits 改标注 / 再做一条的可选覆盖项：字段不出现 = 沿用（refImages 出现即整体替换，含空数组）。
type planEdits struct {
	RefImages   *[]string `json:"refImages"`
	ModelGroup  *string   `json:"modelGroup"`
	Resolution  *string   `json:"resolution"`
	AspectRatio *string   `json:"aspectRatio"`
}

func (h *handlers) patchAnnotations(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	var body struct {
		Annotations *core.Annotations `json:"annotations"`
		planEdits
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Annotations == nil {
		return nil, xerr.New(xerr.ParamMissing, "缺少 annotations")
	}
	refund, err := h.take(r, ActionAnnotate, uid)
	if err != nil {
		return nil, err
	}
	v, created, err := s.PatchAnnotations(r.Context(), core.PatchReq{UserID: uid, PlanNo: no, Annotations: *body.Annotations,
		RefImages: body.RefImages, ModelGroup: body.ModelGroup, Resolution: body.Resolution, AspectRatio: body.AspectRatio})
	if err != nil || !created {
		refund() // 缺项 / 失败 / 同一改动的重放不占方案次数
	}
	return v, err
}

func (h *handlers) savePlan(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return s.SavePlan(r.Context(), uid, no, body.Name)
}

// savedPlans GET /saved-plans?page=&size=：page 从 1 起，size 默认 20、最大 50。
func (h *handlers) savedPlans(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	q := r.URL.Query()
	page, size := 1, 20
	for name, dst := range map[string]*int{"page": &page, "size": &size} {
		if v := strings.TrimSpace(q.Get(name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, xerr.Param(name + " 不合法")
			}
			*dst = n
		}
	}
	return s.ListSavedPlans(r.Context(), uid, page, size)
}

func (h *handlers) deleteSavedPlan(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	if err := s.DeleteSavedPlan(r.Context(), uid, id); err != nil {
		return nil, err
	}
	return map[string]any{"id": id}, nil
}

func (h *handlers) reuseSavedPlan(_ http.ResponseWriter, r *http.Request, s *core.Service, uid int64) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	var body struct {
		AnalysisNo string `json:"analysisNo"`
		SampleID   int64  `json:"sampleId"`
		planEdits
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	refund, err := h.take(r, ActionAnnotate, uid)
	if err != nil {
		return nil, err
	}
	v, created, err := s.ReuseSavedPlan(r.Context(), core.ReuseReq{UserID: uid, SavedID: id, AnalysisNo: body.AnalysisNo, SampleID: body.SampleID,
		RefImages: body.RefImages, ModelGroup: body.ModelGroup, Resolution: body.Resolution, AspectRatio: body.AspectRatio,
		IdempotencyKey: idemKey(r)})
	if err != nil || !created {
		refund() // 失败 / 同键重放不占方案次数
	}
	return v, err
}
