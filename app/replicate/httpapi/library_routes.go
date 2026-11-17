package httpapi

import (
	"net/http"

	"aigc-local-life/app/replicate/core"
)

// 示例 / 改标注 / 我的方案（docs/api/api-replicate.md §10）。限频口径（在 ops.go）：改标注、再做一条走独立计数 replicate.rate.annotate_*
// （不占「方案」额度；失败 / 重放退回）；
// 示例列表、试看、取方案、保存 / 列表 / 删除不计。

// samples GET /samples?industry=：已启用且已有分析的示例。
func (h *handlers) samples(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	return sc.ListSamples(r.Context(), r.URL.Query().Get("industry"))
}

// trySample POST /samples/{id}/try：共享分析 + 示例成片，零成本（不建 job、不预留、不计限频）。
func (h *handlers) trySample(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return sc.TrySample(r.Context(), id)
}

func (h *handlers) getPlan(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return sc.GetPlan(r.Context(), no)
}

func (h *handlers) getPreview(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	no, err := pathStr(r, "no")
	if err != nil {
		return nil, err
	}
	return sc.GetPreview(r.Context(), no)
}

// planEdits 改标注 / 再做一条的可选覆盖项：字段不出现 = 沿用（refImages 出现即整体替换，含空数组）。
type planEdits struct {
	RefImages   *[]string `json:"refImages"`
	ModelGroup  *string   `json:"modelGroup"`
	Resolution  *string   `json:"resolution"`
	AspectRatio *string   `json:"aspectRatio"`
}

func (h *handlers) patchAnnotations(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
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
	req := core.PatchReq{PlanNo: no, RefImages: body.RefImages, ModelGroup: body.ModelGroup, Resolution: body.Resolution, AspectRatio: body.AspectRatio}
	if body.Annotations != nil {
		req.Annotations = *body.Annotations
	}
	return sc.PatchAnnotations(r.Context(), req, body.Annotations != nil)
}

func (h *handlers) savePlan(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
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
	return sc.SavePlan(r.Context(), no, body.Name)
}

// savedPlans GET /saved-plans?page=&size=：page 从 1 起，size 默认 20、最大 50。
func (h *handlers) savedPlans(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	page, size, err := pageQuery(r)
	if err != nil {
		return nil, err
	}
	return sc.ListSavedPlans(r.Context(), page, size)
}

func (h *handlers) deleteSavedPlan(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	return sc.DeleteSavedPlan(r.Context(), id)
}

func (h *handlers) reuseSavedPlan(_ http.ResponseWriter, r *http.Request, sc *Scope) (any, error) {
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
	return sc.ReuseSavedPlan(r.Context(), core.ReuseReq{SavedID: id, AnalysisNo: body.AnalysisNo, SampleID: body.SampleID,
		RefImages: body.RefImages, ModelGroup: body.ModelGroup, Resolution: body.Resolution, AspectRatio: body.AspectRatio,
		IdempotencyKey: idemKey(r)})
}
