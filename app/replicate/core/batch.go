package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/quote"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 批量做多条（CONTRACT §M）：「这个方案满意了，换 N 个商品 / 模特各做一条」。
//
//   - 只在源方案已有一条成功成片后开放（BATCH_NOT_READY）；N 在 1~10（PARAM_INVALID）；每条直接做全部段（stage=all）。
//   - 每条只换「主替换项」的参考图，其余改法 / 模型 / 清晰度 / 画幅 / 叠字 / 声音完全沿用：
//     主替换项 = 方案里 mode=replace 且带参考图的项，按优先级 人物(model) > 商品(product) > 服装(garment) > 其余按 DimKeys 顺序，取第一个。
//     确定性规则，不看模型输出：同一方案永远换同一项。变体的 ref_images 张数必须等于主替换项引用的图数，按位置原地替换，
//     所以 dims 里的图号（refs）一个都不用改；主替换项引用的图同时被别的项引用 → 换了会连带改别的项，拒绝（PARAM_INVALID）。
//   - 每个变体派生一张子方案（走与改标注同一套 preparePlan：校验 / 参考图解析与归属 / 分段 / 报价 / 指纹；确认前的内容审核对子方案照常生效），
//     子方案不挂 parent_plan_id（避免被「改标注重放」的子方案比对误命中），源方案与子方案的关系记在批次关联表里。
//   - 整批原子预留：N 条任务全部预留成功才一起转排队；任一条失败（价变 / 审核 / 余额不足…）已预留的整批退回、任务置失败，不部分扣。
//   - 幂等：同用户同 idemKey 重放同一批（不再动钱）；子任务幂等键 {idemKey}#{i}（i 从 0 起）。
//   - 漏斗：一次批量确认记一条 batch_confirm（plan_id = 源方案）；批内每条任务照常记一条 confirm_all，
//     让第 1 段做完 / 成片交付等系统事件按任务继承渠道，转化率口径与单条一致（看板要排除批量任务时用批次关联表）。
// ---------------------------------------------------------------------------------------------

// 批量口径常量与 reason。
const (
	MaxBatchVariants = 10

	// ReasonBatchNotReady 源方案还没有成功的成片，批量未开放（409）。
	ReasonBatchNotReady = "BATCH_NOT_READY"
	// ReasonInProgress 同一个 call_id 的批量还在提交中（并发重放），稍后重试。
	ReasonInProgress = "IN_PROGRESS"

	// batchKeyMax 批量幂等键最长（子键 {key}#{i} 要 ≤ 64）。
	batchKeyMax = 58
)

// BatchStore 批次读写（生产实现 app/replicate/repo；可选，不装配时批量接口回 7003）。
type BatchStore interface {
	// InsertBatch 新建批次头；(user_id, request_id) 撞键返回已有那条（created=false）。
	InsertBatch(ctx context.Context, b *model.Batch) (got *model.Batch, created bool, err error)
	GetBatch(ctx context.Context, userID, id int64) (*model.Batch, error)
	GetBatchByRequest(ctx context.Context, userID int64, requestID string) (*model.Batch, error)
	// UpdateBatch 状态 CAS：当前状态在 from 里才改。
	UpdateBatch(ctx context.Context, id int64, from []int8, set map[string]any) (bool, error)
	// AddBatchJob 登记「批次 → 任务」（(batch_id, seq) 或 job_id 撞键忽略）。
	AddBatchJob(ctx context.Context, bj *model.BatchJob) error
	// ListBatchJobs 批次下的任务，按 seq。
	ListBatchJobs(ctx context.Context, batchID int64) ([]*model.BatchJob, error)
	// BatchIDsOfJobs 任务 → 批次号（不属于任何批次的不在结果里）。
	BatchIDsOfJobs(ctx context.Context, jobIDs []int64) (map[int64]int64, error)
}

// WithBatch 装配批量做多条的批次读写；不装配时批量接口回 7003「功能暂未开放」。
func WithBatch(b BatchStore) Option { return func(s *Service) { s.batches = b } }

func (s *Service) needBatch() error {
	if s.batches == nil {
		return xerr.New(xerr.NotReady, "功能暂未开放")
	}
	return nil
}

// BatchVariant 一个变体：替换主替换项的参考图（媒体服务 rid / mr://rid / 本站上传的 http 地址）。
type BatchVariant struct {
	RefImages []string
}

// BatchQuoteRow 一条的报价。
type BatchQuoteRow struct {
	Index  int   `json:"index"` // 变体序号，从 1 起
	Points int64 `json:"points"`
}

// BatchQuoteView 批量报价（只读，不建方案、不动钱）。
type BatchQuoteView struct {
	PlanNo string `json:"planNo"`
	// PlanHash 源方案当前指纹（供同意卡带出；批量确认本身不需要它，Go 用 plan_no 自取）
	PlanHash string          `json:"planHash"`
	Count    int             `json:"count"`
	Rows     []BatchQuoteRow `json:"rows"`
	Total    int64           `json:"total"`
	// MainDim / MainDimLabel 被替换的主替换项（人物 / 商品 / 服装…）；RefsPerVariant 每个变体要给几张图
	MainDim        string    `json:"mainDim"`
	MainDimLabel   string    `json:"mainDimLabel"`
	RefsPerVariant int       `json:"refsPerVariant"`
	ValidUntil     time.Time `json:"validUntil"`
}

// BatchView 一批任务的整体视图。
type BatchView struct {
	BatchID int64        `json:"batchId"`
	PlanNo  string       `json:"planNo"`
	Jobs    []JobSummary `json:"jobs"`
	Total   int64        `json:"total"` // 整批合计点数（确认时的重算值）
	// Status running 有任务没做完 / succeeded 全部成功 / partial 都已结束但有的没成功 / failed 没有一条成功（含提交失败整批回滚）
	Status       string     `json:"status"`
	FailReason   string     `json:"failReason,omitempty"`
	URLExpiresAt *time.Time `json:"urlExpiresAt,omitempty"`
	// Replay 同 call_id 重放 / 方案已确认：返回的是已有批次（不计限频）
	Replay bool `json:"-"`
}

// BatchConfirmReq 批量确认。
type BatchConfirmReq struct {
	UserID         int64
	PlanNo         string
	Variants       []BatchVariant
	ExpectedPoints int64  // 合计点数，必填 >0；与重算不一致 → QUOTE_CHANGED
	IdempotencyKey string // 必填；子任务键 {key}#{i}
}

// mainDimPriority 主替换项优先级：先 人物 > 商品 > 服装，其余按 DimKeys 顺序。
var mainDimPriority = []string{"model", "product", "garment"}

// batchPlan 源方案解析后的批量上下文。
type batchPlan struct {
	parent    *model.Plan
	analysis  *model.Analysis
	ann       Annotations
	refs      []RefImage
	mainDim   string
	positions []int // 主替换项引用的图位置（0 起，去重升序）
}

// mainReplaceDim 取主替换项；没有（方案里没有「换成我的图」的项）返回空。
func mainReplaceDim(ann Annotations) string {
	isMain := func(k string) bool {
		e, ok := ann.Dims[k]
		return ok && hasReplaceRefs(e)
	}
	order := append([]string(nil), mainDimPriority...)
	for _, k := range DimKeys {
		if !contains(order, k) {
			order = append(order, k)
		}
	}
	for _, k := range order {
		if isMain(k) {
			return k
		}
	}
	// 未登记的维度键理论上进不了方案（validate 已拒），按字母序兜底保持确定性
	var rest []string
	for k := range ann.Dims {
		if !contains(order, k) && isMain(k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	if len(rest) > 0 {
		return rest[0]
	}
	return ""
}

// loadBatchPlan 取源方案并做批量前置检查：本人方案 → 已有成功成片（BATCH_NOT_READY）→ 能确定主替换项（PARAM_INVALID）。
func (s *Service) loadBatchPlan(ctx context.Context, userID int64, planNo string) (*batchPlan, error) {
	p, err := s.store.GetPlanByNo(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这个方案")
	}
	notReady := func(why string) error {
		logx.WithContext(ctx).Infof("replicate: 批量拒收 判据=BATCH_NOT_READY user=%d plan=%s 原因=%s", userID, p.PlanNo, why)
		return xerr.NewReason(xerr.Conflict, ReasonBatchNotReady, "这个方案还没有一条做好的成片。先做出一条，满意了再批量")
	}
	if p.JobID == nil {
		return nil, notReady("方案没有任务")
	}
	j, err := s.store.GetJob(ctx, *p.JobID)
	if err != nil {
		return nil, err
	}
	if j == nil || j.UserID != userID || j.Status != model.JobSucceeded {
		return nil, notReady("方案的任务不是成功状态")
	}
	a, err := s.store.GetAnalysis(ctx, p.AnalysisID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, xerr.NewReason(xerr.NotFound, ReasonSourceGone, "这个方案的原片已经不在了。需要重新上传原片")
	}
	bp := &batchPlan{parent: p, analysis: a, refs: presetRefs(p.Refs)}
	if err := json.Unmarshal(p.Annotations, &bp.ann); err != nil {
		return nil, xerr.New(xerr.ServerError, "方案数据异常，请重新生成方案")
	}
	bp.mainDim = mainReplaceDim(bp.ann)
	if bp.mainDim == "" {
		return nil, xerr.Param("这个方案里没有「换成你的图」的项，批量需要有一项换成参考图（商品 / 人物 / 服装）")
	}
	seen := map[int]bool{}
	// 主替换项用了逐套换装：变体的图同时替换各套引用的图位置（editRefs = 维度级 ∪ 各套）
	for _, r := range editRefs(bp.ann.Dims[bp.mainDim]) {
		if r < 1 || r > len(bp.refs) {
			return nil, xerr.New(xerr.ServerError, "方案数据异常，请重新生成方案")
		}
		if !seen[r-1] {
			seen[r-1] = true
			bp.positions = append(bp.positions, r-1)
		}
	}
	sort.Ints(bp.positions)
	// 主替换项引用的图被别的项共用：原地替换会连带改别的项，拒绝（只换主项是批量的承诺）
	for k, e := range bp.ann.Dims {
		if k == bp.mainDim {
			continue
		}
		for _, r := range editRefs(e) {
			if seen[r-1] {
				return nil, xerr.Param(fmt.Sprintf("「%s」和「%s」共用了同一张图，不能只换一项。请先分开用图再批量", DimLabel[bp.mainDim], DimLabel[k]))
			}
		}
	}
	return bp, nil
}

// checkVariants 变体条数 1~10、每条图数 = 主替换项引用的图数。
func (bp *batchPlan) checkVariants(vs []BatchVariant) error {
	if len(vs) < 1 || len(vs) > MaxBatchVariants {
		return xerr.Param(fmt.Sprintf("一批要 1 到 %d 条，现在是 %d 条", MaxBatchVariants, len(vs)))
	}
	for i, v := range vs {
		n := 0
		for _, r := range v.RefImages {
			if strings.TrimSpace(r) != "" {
				n++
			}
		}
		if n != len(bp.positions) || n != len(v.RefImages) {
			return xerr.Param(fmt.Sprintf("第 %d 条要放 %d 张「%s」的图，现在是 %d 张", i+1, len(bp.positions), DimLabel[bp.mainDim], n))
		}
	}
	return nil
}

// variantRaw 一个变体的完整参考图列表：源方案的图原样保留，主替换项的位置换成变体的图。
func (bp *batchPlan) variantRaw(v BatchVariant) []string {
	raw := make([]string, len(bp.refs))
	for i, r := range bp.refs {
		raw[i] = r.Rid
	}
	for i, pos := range bp.positions {
		raw[pos] = strings.TrimSpace(v.RefImages[i])
	}
	return raw
}

// perJobPoints 每条的点数（所有变体的分段与模型一致，价格相同）：按源方案的分段现算全部段合计。
func (s *Service) perJobPoints(ctx context.Context, bp *batchPlan) (int64, QuoteSnapshot, error) {
	segs, err := planSegments(bp.parent)
	if err != nil {
		return 0, QuoteSnapshot{}, err
	}
	q, err := s.quoteSegments(ctx, bp.parent.UserID, bp.parent.ModelGroup, bp.parent.Resolution, segs)
	if err != nil {
		return 0, QuoteSnapshot{}, err
	}
	return q.PointsOf(quote.StageAll), q, nil
}

// QuoteBatch 批量报价：只读（不建方案、不审核、不动钱），返回每条点数与合计。
func (s *Service) QuoteBatch(ctx context.Context, userID int64, planNo string, variants []BatchVariant) (*BatchQuoteView, error) {
	if err := s.needBatch(); err != nil {
		return nil, err
	}
	bp, err := s.loadBatchPlan(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	if err := bp.checkVariants(variants); err != nil {
		return nil, err
	}
	per, q, err := s.perJobPoints(ctx, bp)
	if err != nil {
		return nil, err
	}
	v := &BatchQuoteView{PlanNo: bp.parent.PlanNo, PlanHash: bp.parent.PlanHash, Count: len(variants), Total: per * int64(len(variants)),
		MainDim: bp.mainDim, MainDimLabel: DimLabel[bp.mainDim], RefsPerVariant: len(bp.positions), ValidUntil: q.ValidUntil}
	for i := range variants {
		v.Rows = append(v.Rows, BatchQuoteRow{Index: i + 1, Points: per})
	}
	return v, nil
}

// batchKey 幂等键规整：过长的折叠成固定长度的摘要（确定性，同键同结果），保证 {key}#{i} ≤ 64。
func batchKey(raw string) string {
	k := strings.TrimSpace(raw)
	if len(k) <= batchKeyMax {
		return k
	}
	sum := sha256.Sum256([]byte(k))
	return "b" + hex.EncodeToString(sum[:])[:40]
}

// ExistingBatch 确认前查已有批次（同 call_id 重放）：命中返回批次视图（Replay=true），否则 (nil, nil)。接口层先调它，命中不走限频。
func (s *Service) ExistingBatch(ctx context.Context, userID int64, key string) (*BatchView, error) {
	if s.batches == nil || strings.TrimSpace(key) == "" {
		return nil, nil
	}
	b, err := s.batches.GetBatchByRequest(ctx, userID, batchKey(key))
	if err != nil || b == nil {
		return nil, err
	}
	return s.replayBatch(ctx, b)
}

// replayBatch 已有批次：提交中（并发重放）→ IN_PROGRESS；提交中但超过回收阈值（上次崩在半路，任务由回收器退点）→ 标失败后按失败回放。
func (s *Service) replayBatch(ctx context.Context, b *model.Batch) (*BatchView, error) {
	if b.Status == model.BatchSubmitting {
		if s.now().Sub(b.UpdatedAt) < s.cfg.ReapAfter {
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "这一批正在提交，几秒后再看")
		}
		_, _ = s.batches.UpdateBatch(ctx, b.ID, []int8{model.BatchSubmitting}, map[string]any{
			"status": model.BatchFailed, "fail_reason": "上次提交没有完成，没有扣点数。请重新确认"})
		if cur, err := s.batches.GetBatch(ctx, b.UserID, b.ID); err == nil && cur != nil {
			b = cur
		}
	}
	v, err := s.batchView(ctx, b)
	if v != nil {
		v.Replay = true
	}
	return v, err
}

// GetBatch 本人的一批任务。
func (s *Service) GetBatch(ctx context.Context, userID, batchID int64) (*BatchView, error) {
	if err := s.needBatch(); err != nil {
		return nil, err
	}
	b, err := s.batches.GetBatch(ctx, userID, batchID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这批任务")
	}
	return s.batchView(ctx, b)
}

func (s *Service) batchView(ctx context.Context, b *model.Batch) (*BatchView, error) {
	links, err := s.batches.ListBatchJobs(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	jobs := make([]*model.Job, 0, len(links))
	for _, l := range links {
		j, err := s.store.GetJob(ctx, l.JobID)
		if err != nil {
			return nil, err
		}
		if j != nil {
			jobs = append(jobs, j)
		}
	}
	items, exp := s.summarize(ctx, jobs)
	for i := range items {
		items[i].BatchID = b.ID
	}
	v := &BatchView{BatchID: b.ID, PlanNo: b.PlanNo, Jobs: items, Total: b.TotalPoints, URLExpiresAt: exp, Status: batchStatus(b, jobs)}
	if b.FailReason != nil {
		v.FailReason = *b.FailReason
	}
	return v, nil
}

// batchStatus 整体状态现算：有任务没结束 → running；全部成功 → succeeded；没有一条成功 → failed；其余 partial。提交失败的批次恒为 failed。
func batchStatus(b *model.Batch, jobs []*model.Job) string {
	if b.Status == model.BatchFailed {
		return "failed"
	}
	if b.Status == model.BatchSubmitting || len(jobs) == 0 {
		return "running"
	}
	ok, bad := 0, 0
	for _, j := range jobs {
		switch {
		case j.Status == model.JobSucceeded:
			ok++
		case model.JobFinal(j.Status):
			bad++
		default:
			return "running"
		}
	}
	switch {
	case bad == 0:
		return "succeeded"
	case ok == 0:
		return "failed"
	}
	return "partial"
}

// ConfirmBatch 批量确认：前置检查 → 价格核对 → 派生子方案（含参考图校验 / 归属 / 报价）→ 子方案内容审核（全部过了才往下）→
// 登记批次头 → 逐条预留（stage=all，停在「提交中」）→ 全部成功才一起转排队；任一失败整批退回。
func (s *Service) ConfirmBatch(ctx context.Context, req BatchConfirmReq) (*BatchView, error) {
	if err := s.needBatch(); err != nil {
		return nil, err
	}
	key := batchKey(req.IdempotencyKey)
	if key == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少请求标识，请刷新页面后重试")
	}
	if req.ExpectedPoints <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少本批合计点数（expectedPoints），请刷新页面后重试")
	}
	if v, err := s.ExistingBatch(ctx, req.UserID, key); err != nil || v != nil {
		return v, err
	}
	bp, err := s.loadBatchPlan(ctx, req.UserID, req.PlanNo)
	if err != nil {
		return nil, err
	}
	if err := bp.checkVariants(req.Variants); err != nil {
		return nil, err
	}
	n := len(req.Variants)
	per, _, err := s.perJobPoints(ctx, bp)
	if err != nil {
		return nil, err
	}
	total := per * int64(n)
	if req.ExpectedPoints != total {
		return nil, priceChanged(req.ExpectedPoints, total)
	}
	// 换声音：每条任务各占一条当日额度，整批放不下就整批不做（VOICE_QUOTA_EXHAUSTED，此时还没动钱）；音色等设置原样继承给每一条
	if bp.ann.Audio == AudioRevoice && bp.ann.Voice != nil && s.voices != nil {
		if err := s.voiceQuotaPrecheck(ctx, req.UserID, n); err != nil {
			return nil, err
		}
	}
	// 派生全部子方案（还没动钱：参考图拒收 / 缺项 / 报价失败在这里就挡掉）
	children := make([]*model.Plan, n)
	for i, v := range req.Variants {
		pr := PlanReq{UserID: req.UserID, Annotations: bp.ann, RefImages: bp.variantRaw(v),
			ModelGroup: bp.parent.ModelGroup, Resolution: bp.parent.Resolution, AspectRatio: bp.parent.AspectRatio}
		o := planOpts{sampleID: bp.parent.SampleID, salt: fmt.Sprintf("batch:%s:%s#%d", bp.parent.PlanNo, key, i), knownRids: refRids(bp.parent.Refs)}
		cp, _, _, err := s.preparePlan(ctx, pr, bp.analysis, o)
		if err != nil {
			return nil, err
		}
		got, _, err := s.store.InsertPlan(ctx, cp)
		if err != nil {
			return nil, err
		}
		children[i] = got
	}
	// 内容审核：全部子方案先过一遍，任何一条不通过 / 审核服务故障都整批不做（此时还没建任务、没动钱）
	for _, c := range children {
		if err := s.gateModeration(ctx, c); err != nil {
			return nil, err
		}
	}
	hdr, created, err := s.batches.InsertBatch(ctx, &model.Batch{UserID: req.UserID, PlanID: bp.parent.ID, PlanNo: bp.parent.PlanNo,
		RequestID: key, VariantCount: n, TotalPoints: total, Status: model.BatchSubmitting})
	if err != nil {
		return nil, err
	}
	if !created { // 并发的同键请求抢先登记了批次
		return s.replayBatch(ctx, hdr)
	}
	// 逐条预留（停在提交中）；任一失败整批退回
	var held []*JobView
	for i, c := range children {
		jv, err := s.confirm(ctx, ConfirmReq{UserID: req.UserID, PlanNo: c.PlanNo, PlanHash: c.PlanHash, Stage: quote.StageAll,
			IdempotencyKey: fmt.Sprintf("%s#%d", key, i), ExpectedPoints: per}, true)
		if err != nil {
			s.rollbackBatch(ctx, hdr, held, fmt.Errorf("第 %d 条：%w", i+1, err))
			return nil, err
		}
		held = append(held, jv)
		if err := s.batches.AddBatchJob(ctx, &model.BatchJob{BatchID: hdr.ID, Seq: i, ChildPlanID: c.ID, JobID: jv.JobID}); err != nil {
			s.rollbackBatch(ctx, hdr, held, fmt.Errorf("登记第 %d 条失败：%w", i+1, err))
			return nil, err
		}
		_, _ = s.batches.UpdateBatch(ctx, hdr.ID, []int8{model.BatchSubmitting}, map[string]any{"total_points": total}) // 刷新心跳（updated_at）
	}
	// 全部预留成功：先给每条任务续一次心跳（被回收器动过的 = 整批作废），再统一转排队
	for _, jv := range held {
		if ok, err := s.store.TouchJob(ctx, jv.JobID, []int8{model.JobSubmitting}); err == nil && !ok {
			s.rollbackBatch(ctx, hdr, held, fmt.Errorf("任务 %d 在提交途中被回收", jv.JobID))
			return nil, xerr.New(xerr.ServerError, "没能提交，请稍后重试；如有冻结的点数会自动退回")
		}
	}
	for i, jv := range held {
		ok, err := s.store.TransitJob(ctx, jv.JobID, []int8{model.JobSubmitting}, model.JobQueued, map[string]any{"next_run_at": nil})
		if err != nil || !ok {
			// 心跳刚过就被回收：回收器已退点并置失败，该条会在批次视图里显示失败；其余已排队的照常做（不再回滚已排队的）
			logx.WithContext(ctx).Errorf("replicate: 批量收尾时任务已不在提交中 判据=BATCH_SUBMIT_REAPED batch=%d job=%d ok=%v: %v", hdr.ID, jv.JobID, ok, err)
			continue
		}
		s.funnel(ctx, funnelEvent{Stage: StageConfirmAll, UserID: req.UserID, AnalysisID: bp.analysis.ID, PlanID: children[i].ID, JobID: jv.JobID, Continuity: planContinuity(children[i]),
			Dedupe: fmt.Sprintf("cf:%d", jv.JobID)})
	}
	_, _ = s.batches.UpdateBatch(ctx, hdr.ID, []int8{model.BatchSubmitting}, map[string]any{"status": model.BatchSubmitted})
	s.funnel(ctx, funnelEvent{Stage: StageBatchConfirm, UserID: req.UserID, AnalysisID: bp.analysis.ID, PlanID: bp.parent.ID, Continuity: planContinuity(bp.parent), Dedupe: fmt.Sprintf("bc:%d", hdr.ID)})
	logx.WithContext(ctx).Infof("replicate: 批量确认 user=%d plan=%s batch=%d 条数=%d 合计=%d", req.UserID, bp.parent.PlanNo, hdr.ID, n, total)
	hdr.Status = model.BatchSubmitted
	return s.batchView(ctx, hdr)
}

// rollbackBatch 整批回滚：已预留的任务逐张退回预留单、任务置失败、子方案放回；批次头标失败（记原因）。
// 脱离请求上下文执行（客户端断开也要收干净）。退回出错的预留单记结算待重试（refund:），由既有补扫继续退。
func (s *Service) rollbackBatch(ctx context.Context, hdr *model.Batch, held []*JobView, cause error) {
	dctx, cancel := s.detached(ctx)
	defer cancel()
	logx.WithContext(ctx).Infof("replicate: 批量预留失败，整批回滚 判据=BATCH_ROLLBACK batch=%d 已预留=%d: %v", hdr.ID, len(held), cause)
	for _, jv := range held {
		j, err := s.store.GetJob(dctx, jv.JobID)
		if err != nil || j == nil {
			logx.WithContext(ctx).Errorf("replicate: 回滚取任务失败 判据=BATCH_ROLLBACK_JOB job=%d: %v", jv.JobID, err)
			continue
		}
		if charges, err := s.store.ListCharges(dctx, j.ID); err == nil {
			for _, c := range charges {
				if c.Status == model.ChargeHeld || c.Status == model.ChargePending {
					s.refundCharge(dctx, j.UserID, c, "批量提交失败退回")
				}
			}
		}
		s.abortSubmit(dctx, j, j.PlanID, "这一批没能全部提交，已整批取消，没有扣点数")
	}
	reason := "这一批没能全部提交，没有扣点数"
	if _, msg, ok := xerr.Parse(cause); ok {
		reason = trimRunes(msg, 200)
	}
	_, _ = s.batches.UpdateBatch(dctx, hdr.ID, []int8{model.BatchSubmitting}, map[string]any{"status": model.BatchFailed, "fail_reason": reason})
}

// ParseBatchID 工具入参里的批次号（字符串形态的数字，与结果视图里模型看到的一致）。
func ParseBatchID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, xerr.Param("batch_id 不合法")
	}
	return id, nil
}
