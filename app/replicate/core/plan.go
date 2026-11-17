package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/quote"
	"aigc-local-life/app/replicate/rules/segment"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 每项的处理方式（三态，EXPERIENCE §4.5：不改 / 换成我的图 / 按我写的改）。
const (
	ModeKeep     = "keep"
	ModeReplace  = "replace"
	ModeDescribe = "describe"
)

// 声音策略（默认重新配乐，EXPERIENCE 0.1 #5）。
const (
	AudioRegenerate = "regenerate"
	AudioKeep       = "keep"
	AudioMute       = "mute"
)

// Annotations 用户改动。
type Annotations struct {
	Dims  map[string]DimEdit `json:"dims"`
	Audio string             `json:"audio"`
	// Texts 生成后由系统叠加的文字（店名 / 价格 / 标题；≤4 处，每处 ≤30 字），不交给 AI 画。见 finish.go。
	Texts []TextOverlay `json:"texts,omitempty"`
}

// DimEdit 一项的改法。Refs 是参考图序号（从 1 起，对应 PlanReq.RefImages 的顺序）。
type DimEdit struct {
	Mode string `json:"mode"`
	Note string `json:"note,omitempty"`
	Refs []int  `json:"refs,omitempty"`
	// Occurrences 逐套换装（CONTRACT §O1，只有这一项在视频里出现 ≥2 次时才允许）：第 index 套换成 refs 的图，没列出的套保持原样。
	// 未用时 JSON 不出现，plan_hash 与旧版一致。
	Occurrences []OccurrenceEdit `json:"occurrences,omitempty"`
}

// PlanReq 生成方案（POST /api/v1/replicate/plans）。
type PlanReq struct {
	UserID      int64
	AnalysisNo  string
	Annotations Annotations
	RefImages   []string // 媒体服务 rid 或 mr://{rid}，最多 9 张，顺序即图号
	ModelGroup  string
	Resolution  string // 默认 720p
	AspectRatio string // 空 = 跟随原片
	// SampleID 用示例的共享分析出方案（与 AnalysisNo 二选一，>0 时优先）
	SampleID int64
}

// Gap 结构化缺项（用户口径），前端逐项定位。
type Gap struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// GapsError 方案校验没通过（不生成方案，不占次数）。
type GapsError struct{ Gaps []Gap }

func (e *GapsError) Error() string {
	ms := make([]string, len(e.Gaps))
	for i, g := range e.Gaps {
		ms[i] = g.Message
	}
	return "还差：" + strings.Join(ms, "；")
}

const maxRefImages = 9

// PlanSegmentView 方案里冻结的一段（含提示词，进 plan_hash）。
type PlanSegmentView struct {
	segment.Segment
	CutLabel string `json:"cutLabel,omitempty"`
}

// RefImage 参考图（图号从 1 起）。
type RefImage struct {
	Index int    `json:"index"`
	Rid   string `json:"rid"`
}

// QuoteSnapshot 方案 / 报价接口返回的报价。
type QuoteSnapshot struct {
	quote.Stages
	ValidUntil   time.Time `json:"validUntil"`
	DefaultStage string    `json:"defaultStage"`
}

// CreatePlan 校验 → 分段 → 渲染 → 报价 → plan_hash。同输入同方案（uk_user_hash）。
// 源视频二选一：AnalysisNo（本人的分析）或 SampleID（示例的共享分析，方案本身仍是本人的，正常计费）。
func (s *Service) CreatePlan(ctx context.Context, req PlanReq) (*PlanView, error) {
	var a *model.Analysis
	var sampleID *int64
	if req.SampleID > 0 {
		sa, _, err := s.sampleAnalysis(ctx, req.SampleID)
		if err != nil {
			return nil, err
		}
		a, sampleID = sa, &req.SampleID
	} else {
		got, err := s.store.GetAnalysisByNo(ctx, req.UserID, req.AnalysisNo)
		if err != nil {
			return nil, err
		}
		if got == nil {
			return nil, xerr.New(xerr.NotFound, "找不到这条视频的分析结果，请重新上传")
		}
		a = got
	}
	v, _, err := s.createPlanFrom(ctx, req, a, planOpts{sampleID: sampleID})
	return v, err
}

// planOpts 方案来源的附加信息（示例 / 改标注 / 再做一条）。
type planOpts struct {
	sampleID *int64
	parentID *int64
	// salt 参与 plan_hash：改标注 = "patch:<原方案号>"，再做一条 = "reuse:<保存 id>:<幂等键>"；空 = 普通方案（指纹与旧版一致）
	salt string
	// presetRefs 非 nil 时直接用这组已解析的参考图（沿用原方案 / 保存的图），不再逐张解析
	presetRefs []RefImage
	// knownRids 本人原方案 / 保存里已解析过的 rid：重新传回来（mr://rid）时直接认（它们可能来自 http 拉入，不一定是上传类别）
	knownRids map[string]bool
}

// createPlanFrom 在一份分析上出方案并落库（uk_user_hash 单飞），返回视图与是否新建。
func (s *Service) createPlanFrom(ctx context.Context, req PlanReq, a *model.Analysis, o planOpts) (*PlanView, bool, error) {
	p, an, q, err := s.preparePlan(ctx, req, a, o)
	if err != nil {
		return nil, false, err
	}
	got, created, err := s.store.InsertPlan(ctx, p)
	if err != nil {
		return nil, false, err
	}
	if !created && got.Status == model.PlanReady && s.now().After(got.ExpiresAt) {
		// 同输入撞上已过期的旧方案（uk_user_hash）：刷新有效期与报价快照后复用（方案号、指纹不变），否则「重新生成方案」永远拿到过期的那张
		ok, err := s.store.RefreshPlan(ctx, got.ID, p.ExpiresAt, p.Quote)
		if err != nil {
			return nil, false, err
		}
		logx.WithContext(ctx).Infof("replicate: 过期方案重新生成，刷新有效期 plan=%s 刷新=%v", got.PlanNo, ok)
		if cur, err := s.store.GetPlan(ctx, got.ID); err != nil {
			return nil, false, err
		} else if cur != nil {
			got = cur
		}
	}
	if created {
		stage := planFunnelStage(o)
		s.funnel(ctx, funnelEvent{Stage: stage, UserID: req.UserID, AnalysisID: a.ID, PlanID: got.ID, Multi: funnelMulti(an), Dedupe: fmt.Sprintf("%s:%d", stage, got.ID)})
	}
	v, err := s.fullPlanView(ctx, got, an, q, true)
	return v, created, err
}

// preparePlan 校验 → 分段 → 渲染 → 报价 → plan_hash，得到待落库的方案（不写库）。
func (s *Service) preparePlan(ctx context.Context, req PlanReq, a *model.Analysis, o planOpts) (*model.Plan, *AnalysisResult, QuoteSnapshot, error) {
	if a.Status != model.AnalysisSucceeded {
		return nil, nil, QuoteSnapshot{}, xerr.New(xerr.Conflict, "视频还在分析中，稍等一下")
	}
	anp, err := s.ResolveAnalysis(a) // 读取时按当前规则纠偏
	if err != nil {
		return nil, nil, QuoteSnapshot{}, xerr.New(xerr.ServerError, "方案生成出了点问题，没有扣点数。请重试，或换一个模型")
	}
	an := *anp
	if o.presetRefs != nil {
		// 校验只看图的张数（图号是否越界），用占位串对齐长度
		req.RefImages = make([]string, len(o.presetRefs))
		for i, r := range o.presetRefs {
			req.RefImages[i] = r.Rid
		}
	}
	spec, modelOK := s.modelEnabled(req.ModelGroup)
	resolutions, ratios := Resolutions, AspectRatios
	resolution := strings.ToLower(strings.TrimSpace(req.Resolution))
	if modelOK {
		resolutions, ratios = spec.Resolutions, spec.AspectRatios
		if resolution == "" {
			resolution = spec.DefaultResolution
		}
	}
	if resolution == "" {
		resolution = "720p"
	}
	ratio := strings.TrimSpace(req.AspectRatio)
	ann, gaps := s.validate(req, &an)
	if !contains(resolutions, resolution) {
		logx.WithContext(ctx).Infof("replicate: 方案拒收 判据=RESOLUTION_UNSUPPORTED user=%d 模型=%s 值=%q", req.UserID, req.ModelGroup, req.Resolution)
		gaps = append(gaps, Gap{Code: "RESOLUTION_UNSUPPORTED", Field: "resolution", Message: "这个模型的清晰度只能选 " + strings.Join(resolutions, " / ")})
	}
	if ratio != "" && !contains(ratios, ratio) {
		logx.WithContext(ctx).Infof("replicate: 方案拒收 判据=ASPECT_UNSUPPORTED user=%d 模型=%s 值=%q", req.UserID, req.ModelGroup, ratio)
		gaps = append(gaps, Gap{Code: "ASPECT_UNSUPPORTED", Field: "aspectRatio", Message: "这个模型的画幅只能选 跟随原片 / " + strings.Join(ratios, " / ")})
	}
	if len(req.RefImages) > maxRefImages {
		gaps = append(gaps, Gap{Code: "REF_TOO_MANY", Field: "refImages", Message: "最多放 9 张"})
	}
	if len(gaps) > 0 {
		return nil, nil, QuoteSnapshot{}, &GapsError{Gaps: gaps}
	}
	refs := o.presetRefs
	if refs == nil {
		// 参考图最后解析：http(s) 要经媒体服务拉入（有成本），前面的缺项先挡掉
		var rgaps []Gap
		var err error
		refs, rgaps, err = s.resolveRefs(ctx, req.UserID, req.RefImages, o.knownRids)
		if err != nil {
			return nil, nil, QuoteSnapshot{}, err
		}
		if len(rgaps) > 0 {
			return nil, nil, QuoteSnapshot{}, &GapsError{Gaps: rgaps}
		}
	}
	segs, err := segment.Plan(an.DurationS, candidates(&an), s.cfg.Limits)
	if err != nil {
		return nil, nil, QuoteSnapshot{}, xerr.New(xerr.ParamError, fmt.Sprintf("这条视频的时长不适合拆分，建议剪到 %d 秒内再试", s.cfg.Limits.MaxDurationSec))
	}
	if len(segs) > 1 && s.cutter == nil {
		return nil, nil, QuoteSnapshot{}, xerr.New(xerr.NotReady, fmt.Sprintf("超过 %d 秒的视频要拆成几段生成，这项能力即将开放。可以先剪到 %d 秒内再试",
			s.cfg.Limits.SegCapSec, s.cfg.Limits.SegCapSec))
	}
	if modelOK {
		for _, sg := range segs {
			if sg.GenDuration < spec.MinDurationSec || sg.GenDuration > spec.MaxDurationSec {
				logx.WithContext(ctx).Infof("replicate: 方案拒收 判据=DURATION_UNSUPPORTED user=%d 模型=%s 段时长=%d", req.UserID, req.ModelGroup, sg.GenDuration)
				return nil, nil, QuoteSnapshot{}, &GapsError{Gaps: []Gap{{Code: "DURATION_UNSUPPORTED", Field: "modelGroup", Message: "这条视频的分段时长这个模型做不了，请换一个模型"}}}
			}
		}
	}
	dl := DialectFor(req.ModelGroup)
	views := make([]PlanSegmentView, len(segs))
	prompts := make([]string, len(segs))
	for i, sg := range segs {
		views[i] = PlanSegmentView{Segment: sg, CutLabel: segment.ReasonLabel(sg.CutReason)}
		prompts[i] = RenderDialect(dl, &an, ann, len(refs), sg, len(segs))
	}
	hash := planHash(a.AnalysisNo, an.PromptVersion, dl.Version, ann, refs, req.ModelGroup, resolution, ratio, segs, prompts)
	if o.salt != "" {
		sum := sha256.Sum256([]byte(hash + "|" + o.salt))
		hash = hex.EncodeToString(sum[:])
	}
	q, err := s.quoteSegments(ctx, req.UserID, req.ModelGroup, resolution, segs)
	if err != nil {
		return nil, nil, QuoteSnapshot{}, err
	}
	annJSON, _ := json.Marshal(ann)
	refJSON, _ := json.Marshal(refs)
	segJSON, _ := json.Marshal(views)
	prJSON, _ := json.Marshal(prompts)
	qJSON, _ := json.Marshal(q)
	p := &model.Plan{PlanNo: newNo("VRP"), UserID: req.UserID, AnalysisID: a.ID, SampleID: o.sampleID, ParentID: o.parentID,
		Status: model.PlanReady, ModelGroup: req.ModelGroup, Resolution: resolution, AspectRatio: ratio, Annotations: annJSON, Refs: refJSON,
		Segments: segJSON, Prompts: prJSON, Quote: qJSON, PlanHash: hash, ExpiresAt: s.now().Add(s.cfg.PlanTTL)}
	return p, &an, q, nil
}

// QuotePlan 重新报价（POST /plans/{no}/quote）：价格只来自后端，有效期 10 分钟。
func (s *Service) QuotePlan(ctx context.Context, userID int64, planNo string) (*PlanView, error) {
	p, an, err := s.loadPlan(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	segs, err := planSegments(p)
	if err != nil {
		return nil, err
	}
	q, err := s.quoteSegments(ctx, userID, p.ModelGroup, p.Resolution, segs)
	if err != nil {
		return nil, err
	}
	return s.fullPlanView(ctx, p, an, q, true)
}

func (s *Service) loadPlan(ctx context.Context, userID int64, planNo string) (*model.Plan, *AnalysisResult, error) {
	p, err := s.store.GetPlanByNo(ctx, userID, planNo)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, xerr.New(xerr.NotFound, "找不到这个方案")
	}
	a, err := s.store.GetAnalysis(ctx, p.AnalysisID)
	if err != nil {
		return nil, nil, err
	}
	an := &AnalysisResult{}
	if a != nil {
		if r, err := s.ResolveAnalysis(a); err == nil {
			an = r
		}
	}
	return p, an, nil
}

// quoteSegments 每段按秒报价（pkg/billing.Quote，按模型组分价），汇总成「先做第 1 段 / 其余 / 合计」。
func (s *Service) quoteSegments(ctx context.Context, userID int64, group, resolution string, segs []segment.Segment) (QuoteSnapshot, error) {
	lines := make([]quote.Line, len(segs))
	for i, sg := range segs {
		pts, err := s.ledger.Quote(ctx, s.charge(userID, "", group, resolution, sg.GenDuration))
		if err != nil {
			return QuoteSnapshot{}, err
		}
		lines[i] = quote.Line{Index: sg.Index, Seconds: sg.GenDuration, Points: pts}
	}
	st, err := quote.Summarize(lines)
	if err != nil {
		return QuoteSnapshot{}, err
	}
	return QuoteSnapshot{Stages: st, ValidUntil: s.now().Add(s.cfg.QuoteTTL)}, nil
}

func (s *Service) charge(userID int64, requestID, group, resolution string, seconds int) billing.Charge {
	return billing.Charge{UserID: userID, RequestID: requestID, EntryCode: s.cfg.EntryCode, ModelGroup: group,
		Kind: billing.KindVideo, Seconds: seconds, Resolution: resolution}
}

// validate 规范化标注并给出结构化缺项（DESIGN §5.4：DIM_ABSENT / DIM_MODE / REF_* …）。参考图本身的解析见 resolveRefs。
func (s *Service) validate(req PlanReq, an *AnalysisResult) (Annotations, []Gap) {
	var gaps []Gap
	add := func(code, field, msg string) { gaps = append(gaps, Gap{Code: code, Field: field, Message: msg}) }
	if _, ok := s.modelEnabled(req.ModelGroup); !ok {
		add("MODEL_UNKNOWN", "modelGroup", "这个模型暂时不可用，请换一个")
	}
	ann := Annotations{Dims: map[string]DimEdit{}, Audio: req.Annotations.Audio}
	switch ann.Audio {
	case "":
		ann.Audio = AudioRegenerate
	case AudioRegenerate, AudioKeep, AudioMute:
	default:
		add("AUDIO_MODE", "audio", "声音的处理方式不对，请重新选择")
	}
	changed := 0
	ann.Texts = validateTexts(req.Annotations.Texts, add)
	changed += len(ann.Texts)
	for k, e := range req.Annotations.Dims {
		label, ok := DimLabel[k]
		field := "dims." + k
		if !ok {
			add("DIM_UNKNOWN", field, "有一项没认出来，请刷新后重试")
			continue
		}
		e.Note = strings.TrimSpace(e.Note)
		switch e.Mode {
		case "", ModeKeep:
			continue
		case ModeReplace:
			if !an.Dims[k].Present {
				add("DIM_ABSENT", field, label+"：视频里没有这一项")
			}
			if len(e.Occurrences) > 0 {
				e.Occurrences = s.validateOccurrences(k, label, field, e, an, len(req.RefImages), add)
				e.Refs = nil // 逐套说了算，维度级的图忽略
			} else if len(e.Refs) == 0 {
				switch k {
				case "product":
					add("REF_MISSING", field, "要把商品换掉，需要一张你的商品图。放一张正面清晰的就行")
				case "model":
					add("REF_MISSING", field, "换人需要一张人物正脸照，生成才稳定")
				default:
					add("REF_MISSING", field, label+"：需要放一张图")
				}
			}
			for _, r := range e.Refs {
				if r < 1 || r > len(req.RefImages) {
					add("REF_UNKNOWN", field, label+"：用到的图不在已放的图里，请重新选择")
				}
			}
		case ModeDescribe:
			if len(e.Occurrences) > 0 {
				add("OCCURRENCE_MODE", field, label+"：逐套处理只能用在「换成我的图」上")
				e.Occurrences = nil
			}
			if e.Note == "" {
				add("NOTE_MISSING", field, label+"：写一句话再继续")
			} else if len([]rune(e.Note)) > 200 {
				add("NOTE_TOO_LONG", field, label+"：说明写短一点（200 字以内）")
			}
			e.Refs = nil
		default:
			add("DIM_MODE", field, label+"：请选择 不改 / 换成我的图 / 按我写的改")
			continue
		}
		changed++
		ann.Dims[k] = e
	}
	if changed == 0 && len(gaps) == 0 {
		add("NOTHING_CHANGED", "dims", "勾出你想换的地方，没勾的保持原样")
	}
	return ann, gaps
}

// validateOccurrences 校验并规范化逐套换装（CONTRACT §O1）：模式已由调用方限定为 replace；
// 这一项在分析里必须有 ≥2 次出现，index 在 1..套数内且不重复，每套至少一张图且图号在已放的图里。返回按套号升序的规范化结果。
func (s *Service) validateOccurrences(key, label, field string, e DimEdit, an *AnalysisResult, nRefs int, add func(code, field, msg string)) []OccurrenceEdit {
	n := len(outfitsOf(an.Dims[key]))
	if n < 2 {
		add("OCCURRENCE_UNSUPPORTED", field, label+"：视频里这一项只出现了一次，不能分套处理")
		return nil
	}
	seen := map[int]bool{}
	out := make([]OccurrenceEdit, 0, len(e.Occurrences))
	for _, o := range e.Occurrences {
		switch {
		case o.Index < 1 || o.Index > n:
			add("OCCURRENCE_INDEX", field, fmt.Sprintf("%s：视频里一共 %d 套，没有第 %d 套", label, n, o.Index))
			continue
		case seen[o.Index]:
			add("OCCURRENCE_DUPLICATE", field, fmt.Sprintf("%s：第 %d 套重复了，每套只能指定一次", label, o.Index))
			continue
		}
		seen[o.Index] = true
		if len(o.Refs) == 0 {
			add("REF_MISSING", field, fmt.Sprintf("%s：第 %d 套需要放一张图", label, o.Index))
		}
		for _, r := range o.Refs {
			if r < 1 || r > nRefs {
				add("REF_UNKNOWN", field, fmt.Sprintf("%s：第 %d 套用到的图不在已放的图里，请重新选择", label, o.Index))
				break
			}
		}
		out = append(out, OccurrenceEdit{Index: o.Index, Refs: o.Refs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// 清晰度 / 画幅兜底白名单（模型不在目录里时用；在目录里的按 ModelSpec，画幅空 = 跟随原片）。
var (
	Resolutions  = []string{"480p", "720p", "1080p"}
	AspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"}
)

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// resolveRefs 参考图 → rid（顺序即图号）。每张可以是：
//   - mr://{rid} 或裸 rid：必须是用户上传类别（媒体服务没有 rid → 用户的归属记录，只能限制为「本站上传来源」）；
//   - http(s)：本站存储签发且属于当前用户（URLOwner），再经媒体服务拉入私有桶（PurposeUserInput，幂等键按 用户 + 地址）。
//
// 不合规给结构化缺项（REF_EMPTY / REF_NOT_OWNED / REF_UNREADABLE / REF_UNAVAILABLE）；媒体服务暂时性错误原样返回（稍后重试）。
func (s *Service) resolveRefs(ctx context.Context, userID int64, raw []string, known map[string]bool) ([]RefImage, []Gap, error) {
	var refs []RefImage
	var gaps []Gap
	for i, r := range raw {
		field := fmt.Sprintf("refImages[%d]", i)
		ref := strings.TrimSpace(r)
		switch {
		case ref == "":
			gaps = append(gaps, Gap{Code: "REF_EMPTY", Field: field, Message: "有一张图没能读取，请换一张"})
		case strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://"):
			if len(ref) > maxSourceURLLen || s.urlOwner == nil || !s.urlOwner(ctx, userID, ref) {
				logx.WithContext(ctx).Infof("replicate: 参考图拒收 判据=REF_URL_NOT_OWNED user=%d 序号=%d 校验已装配=%v", userID, i+1, s.urlOwner != nil)
				gaps = append(gaps, Gap{Code: "REF_NOT_OWNED", Field: field, Message: fmt.Sprintf("第 %d 张图不是你在本站上传的，请重新上传", i+1)})
				continue
			}
			if s.media == nil {
				logx.WithContext(ctx).Infof("replicate: 参考图拒收 判据=REF_MEDIA_UNAVAILABLE user=%d 序号=%d", userID, i+1)
				gaps = append(gaps, Gap{Code: "REF_UNAVAILABLE", Field: field, Message: fmt.Sprintf("第 %d 张图暂时没法读取，请稍后再试", i+1)})
				continue
			}
			sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", userID, ref)))
			res, err := s.media.IngestURL(ctx, ref, mediasvc.PurposeUserInput, s.cfg.KeyPrefix+"rpl-ref:"+hex.EncodeToString(sum[:16]))
			if err != nil {
				if permanentMedia(err) {
					logx.WithContext(ctx).Infof("replicate: 参考图拒收 判据=REF_UNREADABLE user=%d 序号=%d: %v", userID, i+1, err)
					gaps = append(gaps, Gap{Code: "REF_UNREADABLE", Field: field, Message: fmt.Sprintf("第 %d 张图没能读取，请换一张", i+1)})
					continue
				}
				return nil, nil, err
			}
			refs = append(refs, RefImage{Index: i + 1, Rid: res.RID})
		default:
			rid := ref
			if x := aigateway.RidOf(ref); x != "" {
				rid = x
			}
			if !known[rid] && !userUploadRID(rid) {
				logx.WithContext(ctx).Infof("replicate: 参考图拒收 判据=REF_RID_NOT_UPLOAD user=%d 序号=%d rid=%q", userID, i+1, rid)
				gaps = append(gaps, Gap{Code: "REF_NOT_OWNED", Field: field, Message: fmt.Sprintf("第 %d 张图不是从本站上传的，请重新上传", i+1)})
				continue
			}
			refs = append(refs, RefImage{Index: i + 1, Rid: rid})
		}
	}
	return refs, gaps, nil
}

// candidates 切点候选：分镜边界 = 镜头切换；台词间隙 ≥ 0.3 秒的中点 = 说话停顿。
// （媒体服务 /video/scene-cuts、/audio/pauses 新算子上线后可替换为帧差 / 静音检测，DESIGN §5.10。）
func candidates(an *AnalysisResult) []segment.Candidate {
	var out []segment.Candidate
	for i, s := range an.Sections {
		if i > 0 {
			out = append(out, segment.Candidate{T: s.T0, Kind: segment.ReasonCut})
		}
	}
	for i := 1; i < len(an.Lines); i++ {
		if gap := an.Lines[i].T0 - an.Lines[i-1].T1; gap >= 0.3 {
			out = append(out, segment.Candidate{T: an.Lines[i-1].T1 + gap/2, Kind: segment.ReasonPause})
		}
	}
	return out
}

// planHash sha256(canonical json{…})：方案内容（不含价格：价格在确认时重算，价变另走 409）。
func planHash(analysisNo, version, renderVersion string, ann Annotations, refs []RefImage, group, resolution, ratio string, segs []segment.Segment, prompts []string) string {
	b, _ := json.Marshal(struct {
		A, V, R       string
		Ann           Annotations
		Refs          []RefImage
		G, Res, Ratio string
		Segs          []segment.Segment
		Prompts       []string
	}{analysisNo, version, renderVersion, ann, refs, group, resolution, ratio, segs, prompts})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func planSegments(p *model.Plan) ([]segment.Segment, error) {
	var views []PlanSegmentView
	if err := json.Unmarshal(p.Segments, &views); err != nil || len(views) == 0 {
		return nil, xerr.New(xerr.ServerError, "方案数据异常，请重新生成方案")
	}
	out := make([]segment.Segment, len(views))
	for i, v := range views {
		out[i] = v.Segment
	}
	return out, nil
}
