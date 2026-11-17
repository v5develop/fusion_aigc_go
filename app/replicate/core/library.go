package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件：示例视频（共享只读分析 + 预计算示例成片，DESIGN §5.18）、改标注（PATCH /plans/{no}/annotations，DESIGN §8.2）、
// 我的方案（EXPERIENCE §3.9.2）。契约：docs/api/api-replicate.md §10。
//
// 示例的分析以系统身份（user_id=0）跑在 afv_replicate_analysis：不进任何用户的列表与限频，用户拿不到它的编号，只能经 sampleId 引用；
// 用示例出的方案仍是用户自己的方案行（参考图、模型、报价、审核、确认都按正常流程，正常计费）。
// 示例试看（TrySample）只读：不建 job、不预留点数、不写任何用户表、不计限频。

// 细分 reason。
const (
	ReasonPlanStale      = "PLAN_STALE"
	ReasonSavedPlanLimit = "SAVED_PLAN_LIMIT"
	ReasonSourceGone     = "SOURCE_GONE"
	// ReasonPlanConfirming 原方案已确认 / 正在确认（占用中），不能在它上面改标注。
	ReasonPlanConfirming = "PLAN_CONFIRMING"
)

var (
	// ErrPlanStale 改标注：原方案已被另一次（不同的）改动取代。
	ErrPlanStale = errors.New("replicate: 方案已被取代")
	// ErrPlanConfirming 改标注：原方案已确认 / 占用中（非 READY），不出子方案。
	ErrPlanConfirming = errors.New("replicate: 方案已确认或正在确认")
	// ErrSavedPlanLimit 我的方案已满。
	ErrSavedPlanLimit = errors.New("replicate: 我的方案已满")
)

const planConfirmingText = "这条方案正在生成，生成完可以基于它再做一条"

func errPlanConfirming() error {
	return xerr.NewReason(xerr.Conflict, ReasonPlanConfirming, planConfirmingText)
}

func errPlanStale() error {
	return xerr.NewReason(xerr.Conflict, ReasonPlanStale, "方案刚更新了，请再看一眼价格")
}

// SampleSyncStore 惰性发布示例分析用到的读写（core.Store + LibraryStore 的子集；admin 也用）。
type SampleSyncStore interface {
	GetAnalysis(ctx context.Context, id int64) (*model.Analysis, error)
	GetSample(ctx context.Context, id int64) (*model.Sample, error)
	// PromoteSampleAnalysis 预分析成功：analysis_id = pending、pending = NULL、analysis_error = NULL、analyzed_at = 现在
	// WHERE id=? AND pending_analysis_id=?。
	PromoteSampleAnalysis(ctx context.Context, sampleID, pendingID int64) (bool, error)
	// FailSampleAnalysis 预分析失败：pending = NULL、analysis_error = reason WHERE id=? AND pending_analysis_id=?（已发布的不动）。
	FailSampleAnalysis(ctx context.Context, sampleID, pendingID int64, reason string) (bool, error)
}

// LibraryStore 示例 / 改标注 / 我的方案的读写（生产实现 app/replicate/repo）。
type LibraryStore interface {
	SampleSyncStore
	// ListVisibleSamples 启用、未删除的示例（industry 非空时 = 该行业 + 通用），按 sort_order、id。
	ListVisibleSamples(ctx context.Context, industry string, limit int) ([]*model.Sample, error)

	// SupersedePlan 改标注（同一事务，锁住原方案）：
	//   - 原方案 STALE：取代它的那张与 child 指纹相同 → 返回那张（重放）；否则 ErrPlanStale；
	//   - 原方案不是 READY（已确认 / 占用中）→ ErrPlanConfirming，不插入；
	//   - 插入 child（uk_user_hash 撞键返回已有那张）；原方案 READY → STALE + superseded_by = child。
	// created=false 表示返回的是已有的那张（重放）。
	SupersedePlan(ctx context.Context, parentID int64, child *model.Plan) (got *model.Plan, created bool, err error)

	// SavePlan 保存（按用户串行：先锁用户行再数）。同一 plan 已保存 → 返回已有（rename 时改名），created=false；
	// 新建时已有 ≥ max 个 → ErrSavedPlanLimit。
	SavePlan(ctx context.Context, sp *model.SavedPlan, max int, rename bool) (got *model.SavedPlan, created bool, err error)
	GetSavedPlan(ctx context.Context, userID, id int64) (*model.SavedPlan, error)
	ListSavedPlans(ctx context.Context, userID int64, offset, limit int) ([]*model.SavedPlan, int64, error)
	DeleteSavedPlan(ctx context.Context, userID, id int64) (bool, error)
	// TouchSavedPlan 「用它做」一次：use_count + 1、last_used_at = 现在。
	TouchSavedPlan(ctx context.Context, userID, id int64) error
	// ChildPlans 由这张方案改标注出来的子方案（parent_plan_id = parentID，本人），新的在前。
	ChildPlans(ctx context.Context, parentID int64) ([]*model.Plan, error)
	// ExistingAnalyses 这些分析行还在不在（id → true）。
	ExistingAnalyses(ctx context.Context, ids []int64) (map[int64]bool, error)
}

// WithLibrary 装配示例 / 改标注 / 我的方案。不装配时这些接口回 7003。
func WithLibrary(l LibraryStore) Option { return func(s *Service) { s.lib = l } }

func (s *Service) needLib() error {
	if s.lib == nil {
		return xerr.New(xerr.NotReady, "功能暂未开放")
	}
	return nil
}

// ---------------------------------------------------------------- 方案视图（含来源 / 取代关系）

// fullPlanView 方案卡 + 审核状态 + 来源信息。moderate=true 时没有结论就当场审（生成 / 报价 / 改标注）；false 只读已有结论（GET）。
func (s *Service) fullPlanView(ctx context.Context, p *model.Plan, an *AnalysisResult, q QuoteSnapshot, moderate bool) (*PlanView, error) {
	pref, _ := s.store.GetPref(ctx, p.UserID)
	if an == nil {
		an = &AnalysisResult{}
	}
	v, err := s.planView(p, an, q, pref != nil && pref.AutoContinue)
	if err != nil || v == nil {
		return v, err
	}
	if a, _ := s.store.GetAnalysis(ctx, p.AnalysisID); a != nil {
		v.AnalysisNo = a.AnalysisNo
	}
	if moderate {
		v.Moderation = s.planModerationView(ctx, p)
	} else {
		v.Moderation = s.planModerationPeek(ctx, p)
	}
	var ann Annotations
	_ = json.Unmarshal(p.Annotations, &ann)
	v.Annotations = &ann
	_ = json.Unmarshal(p.Refs, &v.RefImages)
	if v.RefImages == nil {
		v.RefImages = []RefImage{}
	}
	v.Resolution, v.AspectRatio = p.Resolution, p.AspectRatio
	if p.SampleID != nil {
		v.SampleID = *p.SampleID
	}
	if p.ParentID != nil {
		if pp, _ := s.store.GetPlan(ctx, *p.ParentID); pp != nil && pp.UserID == p.UserID {
			v.ParentPlanNo = pp.PlanNo
		}
	}
	if p.SupersededBy != nil {
		if np, _ := s.store.GetPlan(ctx, *p.SupersededBy); np != nil && np.UserID == p.UserID {
			v.SupersededBy = np.PlanNo
		}
	}
	return v, nil
}

// planModerationPeek 只读已有审核结论（GET /plans/{no} 不触发新的审核调用）。
func (s *Service) planModerationPeek(ctx context.Context, p *model.Plan) *ModerationView {
	if !s.moderationEnabled() {
		return moderationView(nil, false)
	}
	m, err := s.mod.store.GetModeration(ctx, p.ID, p.PlanHash)
	if err != nil {
		return moderationView(nil, true)
	}
	return moderationView(m, true)
}

// GetPlan GET /plans/{no}：本人的方案（报价是方案里的快照；要最新价调 QuotePlan）。
func (s *Service) GetPlan(ctx context.Context, userID int64, planNo string) (*PlanView, error) {
	p, an, err := s.loadPlan(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	var q QuoteSnapshot
	_ = json.Unmarshal(p.Quote, &q)
	return s.fullPlanView(ctx, p, an, q, false)
}

// ---------------------------------------------------------------- 改标注

// PatchReq PATCH /plans/{no}/annotations。指针字段为 nil = 沿用原方案。
type PatchReq struct {
	UserID      int64
	PlanNo      string
	Annotations Annotations
	RefImages   *[]string
	ModelGroup  *string
	Resolution  *string
	AspectRatio *string
}

// PatchAnnotations 在同一份分析上用新标注出新方案（新 plan_hash、新报价、重新审核），原方案 READY → STALE。
// 返回的 bool 表示是否新建了方案（同一改动的重放为 false，接口层据此退回限频额度）。
func (s *Service) PatchAnnotations(ctx context.Context, req PatchReq) (*PlanView, bool, error) {
	v, created, err := s.patchAnnotations(ctx, req)
	return v, created, err
}

func (s *Service) patchAnnotations(ctx context.Context, req PatchReq) (*PlanView, bool, error) {
	if err := s.needLib(); err != nil {
		return nil, false, err
	}
	parent, err := s.store.GetPlanByNo(ctx, req.UserID, req.PlanNo)
	if err != nil {
		return nil, false, err
	}
	if parent == nil {
		return nil, false, xerr.New(xerr.NotFound, "找不到这个方案")
	}
	a, err := s.store.GetAnalysis(ctx, parent.AnalysisID)
	if err != nil {
		return nil, false, err
	}
	if a == nil {
		return nil, false, xerr.NewReason(xerr.NotFound, ReasonSourceGone, "这个方案的原片已经不在了。需要重新上传原片")
	}
	if parent.SampleID != nil {
		if err := s.checkSampleCurrent(ctx, *parent.SampleID, parent.AnalysisID); err != nil {
			return nil, false, err
		}
	}
	pr := PlanReq{UserID: req.UserID, Annotations: req.Annotations, ModelGroup: parent.ModelGroup, Resolution: parent.Resolution, AspectRatio: parent.AspectRatio}
	if req.ModelGroup != nil {
		pr.ModelGroup = *req.ModelGroup
		if spec, ok := specOf(pr.ModelGroup); ok && req.Resolution == nil && !contains(spec.Resolutions, strings.ToLower(pr.Resolution)) {
			pr.Resolution = spec.DefaultResolution // 换模型且新模型没有原清晰度（如 H3 没有 720p）：用新模型默认档
		}
	}
	if req.Resolution != nil {
		pr.Resolution = *req.Resolution
	}
	if req.AspectRatio != nil {
		pr.AspectRatio = *req.AspectRatio
	}
	o := planOpts{sampleID: parent.SampleID, parentID: &parent.ID, salt: "patch:" + parent.PlanNo}
	if req.RefImages != nil {
		pr.RefImages, o.knownRids = *req.RefImages, refRids(parent.Refs)
	} else {
		o.presetRefs = presetRefs(parent.Refs)
	}
	// 重放先查：已有子方案与这次改动相同 → 直接返回（不再解析参考图 / 报价 / 审核，重放不打下游）
	hit, err := s.existingEdit(ctx, parent, a, pr, o, req.RefImages)
	if err != nil {
		return nil, false, err
	}
	if hit != nil {
		logx.WithContext(ctx).Infof("replicate: 改标注重放 user=%d %s → %s（命中已有子方案）", req.UserID, parent.PlanNo, hit.PlanNo)
		v, err := s.storedPlanView(ctx, hit, a)
		return v, false, err
	}
	if parent.Status == model.PlanStale {
		logx.WithContext(ctx).Infof("replicate: 改标注拒收 判据=PLAN_STALE user=%d plan=%s（已被另一次改动取代）", req.UserID, parent.PlanNo)
		return nil, false, errPlanStale()
	}
	if parent.Status != model.PlanReady {
		logx.WithContext(ctx).Infof("replicate: 改标注拒收 判据=PLAN_CONFIRMING user=%d plan=%s 状态=%s", req.UserID, parent.PlanNo, parent.Status)
		return nil, false, errPlanConfirming()
	}
	child, an, q, err := s.preparePlan(ctx, pr, a, o)
	if err != nil {
		return nil, false, err
	}
	got, created, err := s.lib.SupersedePlan(ctx, parent.ID, child)
	if errors.Is(err, ErrPlanConfirming) {
		logx.WithContext(ctx).Infof("replicate: 改标注拒收 判据=PLAN_CONFIRMING user=%d plan=%s（并发确认已占用）", req.UserID, parent.PlanNo)
		return nil, false, errPlanConfirming()
	}
	if errors.Is(err, ErrPlanStale) {
		logx.WithContext(ctx).Infof("replicate: 改标注拒收 判据=PLAN_STALE user=%d plan=%s（已被另一次改动取代）", req.UserID, parent.PlanNo)
		return nil, false, errPlanStale()
	}
	if err != nil {
		return nil, false, err
	}
	logx.WithContext(ctx).Infof("replicate: 改标注 user=%d %s → %s 原状态=%s", req.UserID, parent.PlanNo, got.PlanNo, parent.Status)
	v, err := s.fullPlanView(ctx, got, an, q, true)
	return v, created, err
}

// existingEdit 这张方案改出来的子方案里，有没有与这次改动完全相同的（取代它的那张优先）。只做本地校验与比较：
// 标注按同一套 validate 规范化后比；参考图比 rid 序列（http 地址要经媒体服务拉入才知道 rid，不当作相同，走完整流程）；
// 模型 / 清晰度 / 画幅比规范化后的值。
func (s *Service) existingEdit(ctx context.Context, parent *model.Plan, a *model.Analysis, pr PlanReq, o planOpts, rawRefs *[]string) (*model.Plan, error) {
	kids, err := s.lib.ChildPlans(ctx, parent.ID)
	if err != nil || len(kids) == 0 {
		return nil, err
	}
	if parent.SupersededBy != nil {
		for i, k := range kids {
			if k.ID == *parent.SupersededBy {
				kids[0], kids[i] = kids[i], kids[0]
			}
		}
	}
	an, err := s.ResolveAnalysis(a)
	if err != nil {
		return nil, nil
	}
	var want []string
	if rawRefs == nil {
		for _, r := range o.presetRefs {
			want = append(want, r.Rid)
		}
	} else {
		for _, raw := range *rawRefs {
			r := strings.TrimSpace(raw)
			if strings.HasPrefix(r, "http://") || strings.HasPrefix(r, "https://") {
				return nil, nil
			}
			if x := aigateway.RidOf(r); x != "" {
				r = x
			}
			want = append(want, r)
		}
	}
	req := pr
	req.RefImages = want
	ann, gaps := s.validate(req, an)
	if len(gaps) > 0 {
		return nil, nil
	}
	wantAnn, _ := json.Marshal(ann)
	res := strings.ToLower(strings.TrimSpace(pr.Resolution))
	if res == "" {
		res = "720p"
	}
	ratio := strings.TrimSpace(pr.AspectRatio)
	for _, k := range kids {
		if k.UserID != parent.UserID || k.ModelGroup != pr.ModelGroup || k.Resolution != res || k.AspectRatio != ratio {
			continue
		}
		var ka Annotations
		if json.Unmarshal(k.Annotations, &ka) != nil {
			continue
		}
		if b, _ := json.Marshal(ka); string(b) != string(wantAnn) {
			continue
		}
		var kr []RefImage
		_ = json.Unmarshal(k.Refs, &kr)
		if len(kr) != len(want) {
			continue
		}
		same := true
		for i := range kr {
			same = same && kr[i].Rid == want[i]
		}
		if same {
			return k, nil
		}
	}
	return nil, nil
}

// storedPlanView 已有方案的视图（报价用方案里的快照、审核只读已有结论）：重放路径不打下游。
func (s *Service) storedPlanView(ctx context.Context, p *model.Plan, a *model.Analysis) (*PlanView, error) {
	an := &AnalysisResult{}
	if a != nil {
		if r, err := s.ResolveAnalysis(a); err == nil {
			an = r
		}
	}
	var q QuoteSnapshot
	_ = json.Unmarshal(p.Quote, &q)
	return s.fullPlanView(ctx, p, an, q, false)
}

// presetRefs 方案 / 保存里的参考图（已解析过的 rid），非 nil（没有图也返回空切片，表示「沿用」）。
func presetRefs(raw json.RawMessage) []RefImage {
	var refs []RefImage
	_ = json.Unmarshal(raw, &refs)
	out := make([]RefImage, 0, len(refs))
	for i, r := range refs {
		out = append(out, RefImage{Index: i + 1, Rid: r.Rid})
	}
	return out
}

// refRids 方案 / 保存里已解析过的参考图 rid 集合。
func refRids(raw json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for _, r := range presetRefs(raw) {
		if r.Rid != "" {
			out[r.Rid] = true
		}
	}
	return out
}

// ---------------------------------------------------------------- 示例（用户侧）

// SampleCard 示例列表里的一项。
type SampleCard struct {
	ID        int64   `json:"id"`
	Title     string  `json:"title"`
	Industry  string  `json:"industry"`
	CoverRid  string  `json:"coverRid,omitempty"`
	CoverURL  string  `json:"coverUrl,omitempty"`
	DurationS float64 `json:"durationS"`
	Summary   string  `json:"summary"`
	HasResult bool    `json:"hasResult"`
}

// SampleList GET /samples。
type SampleList struct {
	Items        []SampleCard `json:"items"`
	URLExpiresAt *time.Time   `json:"urlExpiresAt,omitempty"`
}

// SampleTry POST /samples/{id}/try：共享分析 + 示例原片 + 预计算示例成片，零成本。
type SampleTry struct {
	Sample       SampleCard      `json:"sample"`
	SourceRid    string          `json:"sourceRid"`
	SourceURL    string          `json:"sourceUrl,omitempty"`
	ResultRid    string          `json:"resultRid,omitempty"`
	ResultURL    string          `json:"resultUrl,omitempty"`
	Analysis     *AnalysisResult `json:"analysis"`
	Annotations  *Annotations    `json:"annotations"`
	FreeText     string          `json:"freeText"`
	NextText     string          `json:"nextText"`
	URLExpiresAt *time.Time      `json:"urlExpiresAt,omitempty"`
}

const (
	sampleFreeText = "这是示例效果，用的是平台自有素材，不花点数；换成你的视频，先看报价再决定"
	sampleNextText = "换成你的素材，按这个改法做同款"
	sampleGoneText = "这个示例暂时不能用了，换一个试试"
)

const maxSamples = 50

// ListSamples 已启用且已有分析的示例（不计限频）。
func (s *Service) ListSamples(ctx context.Context, industry string) (*SampleList, error) {
	if err := s.needLib(); err != nil {
		return nil, err
	}
	list, err := s.lib.ListVisibleSamples(ctx, strings.TrimSpace(industry), maxSamples)
	if err != nil {
		return nil, err
	}
	out := &SampleList{Items: []SampleCard{}}
	var rids []string
	for _, smp := range list {
		smp, err := SyncSample(ctx, s.lib, smp)
		if err != nil {
			return nil, err
		}
		if smp == nil || smp.AnalysisID == nil {
			continue
		}
		a, err := s.store.GetAnalysis(ctx, *smp.AnalysisID)
		if err != nil {
			return nil, err
		}
		an, ok := s.sampleResult(a)
		if !ok {
			continue
		}
		c := sampleCard(smp, an)
		out.Items = append(out.Items, c)
		rids = append(rids, c.CoverRid)
	}
	if urls, exp := s.signRIDs(ctx, rids); urls != nil {
		for i := range out.Items {
			out.Items[i].CoverURL = urls[out.Items[i].CoverRid]
		}
		out.URLExpiresAt = exp
	}
	return out, nil
}

// TrySample 用示例体验：只读（不建 job、不预留点数、不写用户表、不计限频）。
func (s *Service) TrySample(ctx context.Context, id int64) (*SampleTry, error) {
	if err := s.needLib(); err != nil {
		return nil, err
	}
	a, smp, err := s.sampleAnalysis(ctx, id)
	if err != nil {
		return nil, err
	}
	an, _ := s.sampleResult(a)
	v := &SampleTry{Sample: sampleCard(smp, an), SourceRid: smp.SourceRid, ResultRid: deref(smp.ResultRid), Analysis: userAnalysis(an),
		FreeText: sampleFreeText, NextText: sampleNextText}
	if len(smp.Annotations) > 0 {
		var ann Annotations
		if json.Unmarshal(smp.Annotations, &ann) == nil {
			v.Annotations = &ann
		}
	}
	if urls, exp := s.signRIDs(ctx, []string{v.SourceRid, v.ResultRid, v.Sample.CoverRid}); urls != nil {
		v.SourceURL, v.ResultURL, v.Sample.CoverURL = urls[v.SourceRid], urls[v.ResultRid], urls[v.Sample.CoverRid]
		v.URLExpiresAt = exp
	}
	return v, nil
}

// sampleAnalysis 示例已发布的分析（示例须启用、未删除、有成功的系统分析）。
func (s *Service) sampleAnalysis(ctx context.Context, id int64) (*model.Analysis, *model.Sample, error) {
	if err := s.needLib(); err != nil {
		return nil, nil, err
	}
	smp, err := s.lib.GetSample(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if smp == nil || smp.Deleted || !smp.Enabled {
		return nil, nil, xerr.New(xerr.NotFound, sampleGoneText)
	}
	if smp, err = SyncSample(ctx, s.lib, smp); err != nil {
		return nil, nil, err
	}
	if smp == nil || smp.AnalysisID == nil {
		return nil, nil, xerr.New(xerr.NotFound, sampleGoneText)
	}
	a, err := s.store.GetAnalysis(ctx, *smp.AnalysisID)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := s.sampleResult(a); !ok {
		logx.WithContext(ctx).Errorf("replicate: 示例分析不可用 判据=SAMPLE_ANALYSIS_INVALID sample=%d analysis=%d", smp.ID, *smp.AnalysisID)
		return nil, nil, xerr.New(xerr.NotFound, sampleGoneText)
	}
	return a, smp, nil
}

const sampleOffText = "这个示例已下架，换一个试试"

// checkSampleCurrent 基于示例的方案再用（改标注 / 再做一条）前核对：示例未删除、未停用，且方案引用的分析行仍是示例当前发布的那一行
// （示例换原片 / 重新分析 / 下架后，旧分析不能再拿来出新方案）。不通过 → 404 SOURCE_GONE。已确认的 job 输入已冻结，不受影响。
func (s *Service) checkSampleCurrent(ctx context.Context, sampleID, analysisID int64) error {
	smp, err := s.lib.GetSample(ctx, sampleID)
	if err != nil {
		return err
	}
	if smp != nil && !smp.Deleted && smp.Enabled {
		if smp, err = SyncSample(ctx, s.lib, smp); err != nil {
			return err
		}
	}
	if smp == nil || smp.Deleted || !smp.Enabled || smp.AnalysisID == nil || *smp.AnalysisID != analysisID {
		logx.WithContext(ctx).Infof("replicate: 示例方案再用拒收 判据=SAMPLE_NOT_CURRENT sample=%d analysis=%d", sampleID, analysisID)
		return xerr.NewReason(xerr.NotFound, ReasonSourceGone, sampleOffText)
	}
	return nil
}

// sampleResult 系统分析行（user_id=0、成功）的结果（读取时按当前规则纠偏）。
func (s *Service) sampleResult(a *model.Analysis) (*AnalysisResult, bool) {
	if a == nil || a.UserID != model.SystemUserID || a.Status != model.AnalysisSucceeded {
		return nil, false
	}
	an, err := s.ResolveAnalysis(a)
	if err != nil {
		return nil, false
	}
	return an, true
}

func sampleCard(smp *model.Sample, an *AnalysisResult) SampleCard {
	c := SampleCard{ID: smp.ID, Title: smp.Title, Industry: smp.Industry, CoverRid: deref(smp.CoverRid), HasResult: deref(smp.ResultRid) != ""}
	if an != nil {
		c.DurationS, c.Summary = an.DurationS, an.Summary
	}
	return c
}

// SyncSample 惰性发布：正在跑的预分析成功 → 替换已发布的；失败 → 只记原因。返回最新的示例行（CAS，多实例并发安全）。
func SyncSample(ctx context.Context, st SampleSyncStore, smp *model.Sample) (*model.Sample, error) {
	if smp == nil || smp.PendingAnalysisID == nil {
		return smp, nil
	}
	pid := *smp.PendingAnalysisID
	a, err := st.GetAnalysis(ctx, pid)
	if err != nil {
		return nil, err
	}
	switch {
	case a == nil:
		if _, err := st.FailSampleAnalysis(ctx, smp.ID, pid, "预分析记录不见了，请重新预分析"); err != nil {
			return nil, err
		}
	case a.Status == model.AnalysisSucceeded:
		ok, err := st.PromoteSampleAnalysis(ctx, smp.ID, pid)
		if err != nil {
			return nil, err
		}
		if ok {
			logx.WithContext(ctx).Infof("replicate: 示例分析已发布 sample=%d analysis=%s", smp.ID, a.AnalysisNo)
		}
	case a.Status == model.AnalysisFailed:
		reason := strings.TrimSpace(deref(a.ErrorMsg))
		if reason == "" {
			reason = "预分析没成功"
		}
		logx.WithContext(ctx).Infof("replicate: 示例预分析失败 判据=%s sample=%d analysis=%s", deref(a.ErrorCode), smp.ID, a.AnalysisNo)
		if _, err := st.FailSampleAnalysis(ctx, smp.ID, pid, trimRunes(reason, 200)); err != nil {
			return nil, err
		}
	default:
		return smp, nil // 还在跑
	}
	return st.GetSample(ctx, smp.ID)
}

// NewSampleAnalysis 示例的一轮预分析（系统身份 user_id=0）：source_key = sha256(sample:<id>:<轮次>)，每轮新的一行；
// source_url = mr://rid（ingest 阶段直接用 rid，不再拉入）。state 留空：由轮询器首次推进时按 gateway 的运行参数冻结。
func NewSampleAnalysis(sampleID int64, round int, sourceRid string) *model.Analysis {
	sum := sha256.Sum256([]byte(fmt.Sprintf("sample:%d:%d", sampleID, round)))
	return &model.Analysis{AnalysisNo: newNo("VRA"), UserID: model.SystemUserID, SourceKey: hex.EncodeToString(sum[:]),
		SourceURL: aigateway.MRScheme + sourceRid, Status: model.AnalysisQueued, Stage: model.StageIngest}
}

// ValidSampleAnnotations 示例改法的结构校验（后台登记时用）：只认十一项 key、三种改法、三种声音策略。
func ValidSampleAnnotations(raw json.RawMessage) (*Annotations, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var ann Annotations
	if err := json.Unmarshal(raw, &ann); err != nil {
		return nil, errors.New("改法不是合法的 JSON 对象")
	}
	for k, e := range ann.Dims {
		if _, ok := DimLabel[k]; !ok {
			return nil, fmt.Errorf("改法里有不认识的项：%s", k)
		}
		switch e.Mode {
		case "", ModeKeep, ModeReplace, ModeDescribe:
		default:
			return nil, fmt.Errorf("%s 的改法只能是 keep / replace / describe", k)
		}
	}
	switch ann.Audio {
	case "", AudioRegenerate, AudioKeep, AudioMute:
	default:
		return nil, errors.New("声音只能是 regenerate / keep / mute")
	}
	return &ann, nil
}

// ---------------------------------------------------------------- 我的方案

// SavedRef 保存的一张参考图。
type SavedRef struct {
	Index int    `json:"index"`
	Rid   string `json:"rid"`
	URL   string `json:"url,omitempty"`
}

// SavedPlanView 我的方案一项。
type SavedPlanView struct {
	ID              int64       `json:"id"`
	Name            string      `json:"name"`
	PlanNo          string      `json:"planNo"`
	Title           string      `json:"title"`
	Labels          []string    `json:"labels"`
	ModelGroup      string      `json:"modelGroup"`
	Resolution      string      `json:"resolution"`
	AspectRatio     string      `json:"aspectRatio"`
	Annotations     Annotations `json:"annotations"`
	RefImages       []SavedRef  `json:"refImages"`
	SourceThumbRid  string      `json:"sourceThumbRid,omitempty"`
	SourceThumbURL  string      `json:"sourceThumbUrl,omitempty"`
	SourceAvailable bool        `json:"sourceAvailable"`
	SampleID        int64       `json:"sampleId,omitempty"`
	UseCount        int         `json:"useCount"`
	LastUsedAt      *time.Time  `json:"lastUsedAt,omitempty"`
	CreatedAt       time.Time   `json:"createdAt"`
	// Created 本次保存是否新建（false = 这张方案已经保存过，只更新了名字）
	Created bool `json:"created"`
}

// SavedPlanList GET /saved-plans。
type SavedPlanList struct {
	Items        []SavedPlanView `json:"items"`
	Total        int64           `json:"total"`
	Page         int             `json:"page"`
	Size         int             `json:"size"`
	Limit        int             `json:"limit"`
	URLExpiresAt *time.Time      `json:"urlExpiresAt,omitempty"`
}

const maxSavedNameRunes = 40

// SavePlan POST /plans/{no}/save：保存为我的方案（每人 ≤50；同一方案只存一份，再存只改名）。
func (s *Service) SavePlan(ctx context.Context, userID int64, planNo, name string) (*SavedPlanView, error) {
	if err := s.needLib(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) > maxSavedNameRunes {
		return nil, xerr.New(xerr.ParamError, fmt.Sprintf("方案名写短一点（%d 字以内）", maxSavedNameRunes))
	}
	p, an, err := s.loadPlan(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	var ann Annotations
	_ = json.Unmarshal(p.Annotations, &ann)
	rename := name != ""
	if !rename {
		name = defaultSavedName(an, ann)
	}
	sp := &model.SavedPlan{UserID: userID, Name: name, PlanID: p.ID, PlanNo: p.PlanNo, AnalysisID: p.AnalysisID, SampleID: p.SampleID,
		Annotations: p.Annotations, Refs: p.Refs, ModelGroup: p.ModelGroup, Resolution: p.Resolution, AspectRatio: p.AspectRatio,
		Title: trimRunes(strings.TrimSpace(an.Summary), 120)}
	if a, err := s.store.GetAnalysis(ctx, p.AnalysisID); err == nil && a != nil {
		var st AnalysisState
		if json.Unmarshal(a.State, &st) == nil {
			if len(st.Frames) > 0 {
				sp.SourceThumbRid = ptr(st.Frames[0].Rid)
			} else if st.SheetRid != "" {
				sp.SourceThumbRid = ptr(st.SheetRid)
			}
		}
	}
	got, created, err := s.lib.SavePlan(ctx, sp, model.MaxSavedPlans, rename)
	if errors.Is(err, ErrSavedPlanLimit) {
		logx.WithContext(ctx).Infof("replicate: 保存方案拒收 判据=SAVED_PLAN_LIMIT user=%d 上限=%d", userID, model.MaxSavedPlans)
		return nil, xerr.NewReason(xerr.Conflict, ReasonSavedPlanLimit, "方案满了，请删除不用的")
	}
	if err != nil {
		return nil, err
	}
	views := s.savedViews(ctx, []*model.SavedPlan{got}, nil)
	v := views[0]
	v.Created = created
	return &v, nil
}

// defaultSavedName 「{第一处改动的现状}·换{项}」，没有改项时「加文字」；≤40 字。
func defaultSavedName(an *AnalysisResult, ann Annotations) string {
	labels := savedLabels(an, ann)
	head := ""
	if an != nil {
		for _, k := range an.DimOrder {
			if e, ok := ann.Dims[k]; ok && e.Mode != ModeKeep && e.Mode != "" {
				head = strings.TrimSpace(an.Dims[k].Desc)
				break
			}
		}
	}
	name := strings.Join(labels, "、")
	if head != "" {
		name = head + "·" + name
	}
	if name == "" {
		name = "我的方案"
	}
	return trimRunes(name, maxSavedNameRunes)
}

// savedLabels 改法标签：换商品 / 改服装（按我写的改）/ 加文字 / 保留原声…（按分析里的项顺序，分析缺失时按 key 顺序）。
func savedLabels(an *AnalysisResult, ann Annotations) []string {
	order := DimKeys
	if an != nil && len(an.DimOrder) > 0 {
		order = an.DimOrder
	}
	labels := []string{}
	for _, k := range order {
		e, ok := ann.Dims[k]
		if !ok {
			continue
		}
		switch e.Mode {
		case ModeReplace:
			labels = append(labels, "换"+DimLabel[k])
		case ModeDescribe:
			labels = append(labels, "改"+DimLabel[k])
		}
	}
	if len(ann.Texts) > 0 {
		labels = append(labels, "加文字")
	}
	if ann.Audio != "" && ann.Audio != AudioRegenerate {
		labels = append(labels, audioLabel[ann.Audio])
	}
	return labels
}

// ListSavedPlans GET /saved-plans：本人的方案（新的在前）。
func (s *Service) ListSavedPlans(ctx context.Context, userID int64, page, size int) (*SavedPlanList, error) {
	if err := s.needLib(); err != nil {
		return nil, err
	}
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	size = min(size, 50)
	list, total, err := s.lib.ListSavedPlans(ctx, userID, (page-1)*size, size)
	if err != nil {
		return nil, err
	}
	out := &SavedPlanList{Total: total, Page: page, Size: size, Limit: model.MaxSavedPlans}
	var exp *time.Time
	out.Items = s.savedViews(ctx, list, &exp)
	out.URLExpiresAt = exp
	return out, nil
}

// savedViews 组装视图：原片还在不在（批量查）、缩略图 / 参考图现签。
func (s *Service) savedViews(ctx context.Context, list []*model.SavedPlan, exp **time.Time) []SavedPlanView {
	ids := make([]int64, 0, len(list))
	for _, sp := range list {
		ids = append(ids, sp.AnalysisID)
	}
	exists := map[int64]bool{}
	if len(ids) > 0 {
		if m, err := s.lib.ExistingAnalyses(ctx, ids); err == nil {
			exists = m
		} else {
			logx.WithContext(ctx).Errorf("replicate: 查原片是否还在失败（按在处理） user 方案数=%d: %v", len(ids), err)
			for _, id := range ids {
				exists[id] = true
			}
		}
	}
	out := make([]SavedPlanView, 0, len(list))
	var rids []string
	for _, sp := range list {
		var ann Annotations
		_ = json.Unmarshal(sp.Annotations, &ann)
		var refs []RefImage
		_ = json.Unmarshal(sp.Refs, &refs)
		v := SavedPlanView{ID: sp.ID, Name: sp.Name, PlanNo: sp.PlanNo, Title: sp.Title, Labels: savedLabels(nil, ann), ModelGroup: sp.ModelGroup,
			Resolution: sp.Resolution, AspectRatio: sp.AspectRatio, Annotations: ann, RefImages: []SavedRef{},
			SourceThumbRid: deref(sp.SourceThumbRid), SourceAvailable: exists[sp.AnalysisID], UseCount: sp.UseCount,
			LastUsedAt: sp.LastUsedAt, CreatedAt: sp.CreatedAt}
		if sp.SampleID != nil {
			v.SampleID = *sp.SampleID
		}
		for i, r := range refs {
			v.RefImages = append(v.RefImages, SavedRef{Index: i + 1, Rid: r.Rid})
			rids = append(rids, r.Rid)
		}
		rids = append(rids, v.SourceThumbRid)
		out = append(out, v)
	}
	if urls, e := s.signRIDs(ctx, rids); urls != nil {
		for i := range out {
			out[i].SourceThumbURL = urls[out[i].SourceThumbRid]
			for j := range out[i].RefImages {
				out[i].RefImages[j].URL = urls[out[i].RefImages[j].Rid]
			}
		}
		if exp != nil {
			*exp = e
		}
	}
	return out
}

// DeleteSavedPlan DELETE /saved-plans/{id}（只删本人的；别人的回「找不到」）。
func (s *Service) DeleteSavedPlan(ctx context.Context, userID, id int64) error {
	if err := s.needLib(); err != nil {
		return err
	}
	ok, err := s.lib.DeleteSavedPlan(ctx, userID, id)
	if err != nil {
		return err
	}
	if !ok {
		return xerr.New(xerr.NotFound, "找不到这个方案，可能已经删除了")
	}
	return nil
}

// ReuseReq POST /saved-plans/{id}/reuse。源视频二选一（都不传 = 保存时的原片）；指针字段 nil = 沿用保存的。
type ReuseReq struct {
	UserID         int64
	SavedID        int64
	AnalysisNo     string
	SampleID       int64
	RefImages      *[]string
	ModelGroup     *string
	Resolution     *string
	AspectRatio    *string
	IdempotencyKey string
}

// ReuseSavedPlan 用保存的改法 + 新的素材 / 源视频出一张新方案（新 plan_hash：带保存 id 与幂等键，同键重放同一张）。
// 返回的 bool 表示是否新建了方案（重放为 false，接口层据此退回限频额度）。
func (s *Service) ReuseSavedPlan(ctx context.Context, req ReuseReq) (*PlanView, bool, error) {
	if err := s.needLib(); err != nil {
		return nil, false, err
	}
	sp, err := s.lib.GetSavedPlan(ctx, req.UserID, req.SavedID)
	if err != nil {
		return nil, false, err
	}
	if sp == nil {
		return nil, false, xerr.New(xerr.NotFound, "找不到这个方案，可能已经删除了")
	}
	var a *model.Analysis
	sampleID := sp.SampleID
	switch {
	case req.SampleID > 0:
		if a, _, err = s.sampleAnalysis(ctx, req.SampleID); err != nil {
			return nil, false, err
		}
		sampleID = &req.SampleID
	case strings.TrimSpace(req.AnalysisNo) != "":
		if a, err = s.store.GetAnalysisByNo(ctx, req.UserID, strings.TrimSpace(req.AnalysisNo)); err != nil {
			return nil, false, err
		}
		if a == nil {
			return nil, false, xerr.New(xerr.NotFound, "找不到这条视频的分析结果，请重新上传")
		}
		sampleID = nil
	default:
		if a, err = s.store.GetAnalysis(ctx, sp.AnalysisID); err != nil {
			return nil, false, err
		}
		// 原片必须还在，且是本人的分析或保存时用的示例分析（防止拿别人的分析行出方案）
		if a == nil || !(a.UserID == req.UserID || (a.UserID == model.SystemUserID && sp.SampleID != nil)) {
			logx.WithContext(ctx).Infof("replicate: 再做一条拒收 判据=SOURCE_GONE user=%d saved=%d analysis=%d", req.UserID, sp.ID, sp.AnalysisID)
			return nil, false, xerr.NewReason(xerr.NotFound, ReasonSourceGone, "原片已经不在了，需要重新上传原片（方案设置会保留）")
		}
		if sp.SampleID != nil && a.UserID == model.SystemUserID {
			if err := s.checkSampleCurrent(ctx, *sp.SampleID, a.ID); err != nil {
				return nil, false, err
			}
		}
	}
	var ann Annotations
	_ = json.Unmarshal(sp.Annotations, &ann)
	pr := PlanReq{UserID: req.UserID, Annotations: ann, ModelGroup: sp.ModelGroup, Resolution: sp.Resolution, AspectRatio: sp.AspectRatio}
	if req.ModelGroup != nil {
		pr.ModelGroup = *req.ModelGroup
		if spec, ok := specOf(pr.ModelGroup); ok && req.Resolution == nil && !contains(spec.Resolutions, strings.ToLower(pr.Resolution)) {
			pr.Resolution = spec.DefaultResolution // 换模型且新模型没有原清晰度（如 H3 没有 720p）：用新模型默认档
		}
	}
	if req.Resolution != nil {
		pr.Resolution = *req.Resolution
	}
	if req.AspectRatio != nil {
		pr.AspectRatio = *req.AspectRatio
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > 64 {
		// 没带幂等键：按「保存 id + 这次的素材」派生（同样的素材重复点只出一张，不随机）
		fp, _ := json.Marshal(struct {
			Saved      int64
			AnalysisNo string
			SampleID   int64
			Refs       *[]string
			Model      *string
			Res, Ratio *string
		}{sp.ID, strings.TrimSpace(req.AnalysisNo), req.SampleID, req.RefImages, req.ModelGroup, req.Resolution, req.AspectRatio})
		sum := sha256.Sum256(fp)
		key = "fp:" + hex.EncodeToString(sum[:16])
	}
	o := planOpts{sampleID: sampleID, salt: fmt.Sprintf("reuse:%d:%s", sp.ID, key)}
	if req.RefImages != nil {
		pr.RefImages, o.knownRids = *req.RefImages, refRids(sp.Refs)
	} else {
		o.presetRefs = presetRefs(sp.Refs)
	}
	v, created, err := s.createPlanFrom(ctx, pr, a, o)
	if err != nil {
		return nil, false, err
	}
	if created {
		if err := s.lib.TouchSavedPlan(ctx, req.UserID, sp.ID); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 记录方案使用次数失败（不影响出方案） saved=%d: %v", sp.ID, err)
		}
	}
	return v, created, nil
}

// SampleRidOf 后台登记示例时的 rid 规范化：收裸 rid 或 mr://rid；只接受后台上传类别（asset：运营上传进素材库桶的资源），
// 不接受用户上传类（-user-，任何用户都能产生，拿来当共享示例等于让别人的文件进所有人的示例）及中间产物 / 交付类。
// 返回规范化后的 rid 与拒收原因（空 = 通过；空串输入返回 ("", "")）。
func SampleRidOf(raw string) (string, string) {
	r := strings.TrimSpace(raw)
	if r == "" {
		return "", ""
	}
	if x := aigateway.RidOf(r); x != "" {
		r = x
	}
	switch {
	case !mediasvc.ValidRID(r):
		return r, "不是合法的媒体 rid"
	case strings.Contains(r, "-user-"):
		return r, "是用户上传的文件，示例只能用后台上传的素材（asset 类 rid）"
	case !strings.Contains(r, "-asset-"):
		return r, "不是后台上传的素材，示例只能用 asset 类 rid"
	}
	return r, ""
}
