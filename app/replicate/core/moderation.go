package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 方案确认前的内容审核（P0-4，DESIGN §10.5，策略与 Java InputModerationService 一致）：
//
//   - 审什么：源视频关键帧（复用分析阶段已抽的帧，不重新抽）→ biz_type=VIDEO；参考图 → IMAGE；
//     叠字文本（店名 / 价格 / 标题）与用户写的改法说明 → TEXT（先过 pkg/moderation 关键词库，再过网关 moderation.text）。
//   - 怎么审：AI 网关规范操作 moderation.image / moderation.text（每一类一次批量调用；网关只给归一化结论 pass/review/block）。
//     策略全在这里：block 恒违规；review 默认按违规（ModerationReviewPass=true 放行）；审核服务故障默认 fail-close
//     （ModerationFailOpen=true 放行并记 ERROR_OPEN）。故障绝不当违规，违规也绝不当故障。
//   - 什么时候审：异步预审——生成 / 重新报价 / 改标注出方案后立即在后台发起（结论按 plan_id + plan_hash 落 afv_replicate_moderation，
//     方案变了就重审），方案不等结论（ModerationPlanWait 默认 0）；确认时已有有效结论直接用，还没有就最多等 ModerationConfirmWait
//     （默认 3 秒），仍没有 → MODERATION_PENDING（不预留点数）。不通过 → 方案不可确认、不预留点数，返回可读原因（不暴露命中词）。
//     同一进程内同一版方案同一口径只跑一个后台审核（单飞）；多实例各自发起时网关幂等键相同，共用同一次网关调用。
//   - 留痕：每次实际审核（通过 / 未通过 / 故障）都写一条 afv_moderation_log（与 Java 同表同口径，provider=gateway / keyword，
//     request_id = replicate:<方案号>）；写日志失败只告警，不影响审核卡点。
// ---------------------------------------------------------------------------------------------

// 网关审核操作（契约同 Java GatewayModerationClient）。
const (
	opModerationImage = "moderation.image"
	opModerationText  = "moderation.text"

	moderationMaxBatch    = 100
	moderationTextMaxRune = 10000
)

// 审核日志的对象类型 / 提供方 / 结果态（afv_moderation_log）。
const (
	ModBizImage = "IMAGE"
	ModBizText  = "TEXT"
	ModBizVideo = "VIDEO"

	ModProviderGateway = "gateway"
	ModProviderKeyword = "keyword"

	ModResultPassed   = "PASSED"
	ModResultRejected = "REJECTED"
	ModResultError    = "ERROR"
)

// ReasonModerationRejected / ReasonModerationUnavailable 确认接口的细分 reason。
const (
	ReasonModerationRejected    = "MODERATION_REJECTED"
	ReasonModerationUnavailable = "MODERATION_UNAVAILABLE"
	ReasonModerationPending     = "MODERATION_PENDING"
)

// 用户口径文案（EXPERIENCE §4.4 / §4.12：说「内容检查」，不说命中了什么词）。
const (
	modTextRef      = "有参考图没有通过内容检查，请换一张再试。（没有扣点数）"
	modTextFrame    = "原视频的画面没有通过内容检查，换一条视频再试。（没有扣点数）"
	modTextWords    = "填写的文字没有通过内容检查，请修改后再试。（没有扣点数）"
	modTextDown     = "内容检查暂时不可用，请稍后再试。（没有扣点数）"
	modTextPassed   = "已通过内容检查"
	modTextPending  = "正在做内容检查，一般几秒内完成"
	modTextWait     = "内容检查还没完成，几秒后再点确认。（没有扣点数）"
	modRequestIDFmt = "replicate:%s"
)

// ModerationStore 审核结论（生产实现 app/replicate/repo，bun）。
type ModerationStore interface {
	GetModeration(ctx context.Context, planID int64, planHash string) (*model.Moderation, error)
	// PutModeration 按 (plan_id, plan_hash) upsert。
	PutModeration(ctx context.Context, m *model.Moderation) error
}

// ModerationLog 一条 afv_moderation_log。
type ModerationLog struct {
	UserID         int64
	BizType        string // IMAGE / TEXT / VIDEO
	Provider       string // gateway / keyword
	Passed         bool
	ResultStatus   string // PASSED / REJECTED / ERROR
	Scene          string
	Label          string
	RejectedTarget string // 命中目标（参考图 #n / 第 n 帧 / 文本摘要；不落签名地址）
	Reason         string
	RequestContent string
	RawResponse    string
	RequestID      string // replicate:<方案号>
	OutTaskID      string // 网关调用号（多个以 ", " 拼接）
}

// ModerationLogger 审核留痕（生产实现写 afv_moderation_log；失败只告警）。
type ModerationLogger interface {
	LogModeration(ctx context.Context, l ModerationLog) error
}

// WithModeration 装配方案审核。store 必填；logger 可空（不落审核日志）；words 为关键词库（可空）。
// 不装配时（store 为 nil）方案不审核（测试 / 本地）。
func WithModeration(store ModerationStore, logger ModerationLogger, words moderation.Checker) Option {
	return func(s *Service) {
		if store == nil {
			return
		}
		s.mod = &moderator{store: store, logger: logger, words: words}
	}
}

type moderator struct {
	store  ModerationStore
	logger ModerationLogger
	words  moderation.Checker

	mu      sync.Mutex
	flights map[string]*modFlight // 进行中的后台审核（plan_id|plan_hash|confirming → 单飞）
	bg      sync.WaitGroup        // 后台审核（测试等它收尾）
}

// modFlight 一次后台审核；done 关闭后 m / err 可读。
type modFlight struct {
	done chan struct{}
	m    *model.Moderation
	err  error
}

// ModerationView PlanView.moderation：方案能不能确认（前端据 status 禁用确认钮并展示 text）。
type ModerationView struct {
	Status string `json:"status"` // passed / rejected / pending（还没审，确认时审）/ skipped（未开启）
	Text   string `json:"text,omitempty"`
}

func moderationView(m *model.Moderation, enabled bool) *ModerationView {
	switch {
	case !enabled:
		return &ModerationView{Status: "skipped"}
	case m == nil:
		return &ModerationView{Status: "pending", Text: modTextPending}
	case m.Status == model.ModerationRejected:
		return &ModerationView{Status: "rejected", Text: m.Reason}
	}
	return &ModerationView{Status: "passed", Text: modTextPassed}
}

func (s *Service) moderationEnabled() bool { return s.mod != nil && !s.cfg.ModerationOff }

// errModerationDown 审核服务故障（网关 FAILED / 截止内没到终态 / 传输错误 / 结果读不懂），不是违规。
type errModerationDown struct {
	check  string
	callID string
	raw    string
}

func (e *errModerationDown) Error() string { return "审核服务故障 判据=" + e.check }

// modItem 一条结论（与输入同序）。
type modItem struct {
	Verdict string     `json:"verdict"`
	Labels  []modLabel `json:"labels,omitempty"`
}

type modLabel struct {
	Label       string   `json:"label,omitempty"`
	SubLabel    string   `json:"sub_label,omitempty"`
	VendorLabel string   `json:"vendor_label,omitempty"`
	Score       *float64 `json:"score,omitempty"`
}

// modBatch 一类的审核结果。
type modBatch struct {
	items   []modItem
	callIDs []string
	raw     string
}

// moderationKey 网关幂等键：mod:<方案号>:<类>:sha256(策略签名 | 重审轮次 | 输入)。同一版方案同一轮重放同一结论；
// 输入、策略（模型组等）或重审轮次（ERROR_OPEN 确认时重审、REJECTED 过期重审）任一变化都是新键、新调用。
func moderationKey(prefix, planNo, kind, sig string, attempt int, inputs []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|", sig, attempt)
	for _, in := range inputs {
		fmt.Fprintf(h, "%d:%s", len(in), in)
	}
	return prefix + "mod:" + planNo + ":" + kind + ":" + hex.EncodeToString(h.Sum(nil))
}

// gatewayModerate 一类一批（超过 100 条分批，共用一个整体截止）。images=true 时 inputs 是 https 地址。
func (s *Service) gatewayModerate(ctx context.Context, planNo, kind, sig string, attempt int, images bool, inputs []string) (*modBatch, error) {
	if s.gw == nil {
		return nil, &errModerationDown{check: "gateway.not_configured"}
	}
	op, group, budget := opModerationText, s.cfg.ModerationTextGroup, s.cfg.ModerationTextDeadline
	if images {
		op, group, budget = opModerationImage, s.cfg.ModerationImageGroup, s.cfg.ModerationImageDeadline
	}
	deadline := time.Now().Add(budget)
	out := &modBatch{}
	var raws []string
	for from := 0; from < len(inputs); from += moderationMaxBatch {
		to := min(len(inputs), from+moderationMaxBatch)
		part := inputs[from:to]
		var input any
		if images {
			list := make([]map[string]any, len(part))
			for i, u := range part {
				list[i] = map[string]any{"image": aigateway.MediaRef{URI: u}, "data_id": fmt.Sprintf("img-%d", from+i)}
			}
			input = map[string]any{"images": list}
		} else {
			list := make([]map[string]any, len(part))
			for i, t := range part {
				if r := []rune(t); len(r) > moderationTextMaxRune {
					t = string(r[:moderationTextMaxRune])
				}
				list[i] = map[string]any{"content": t, "data_id": fmt.Sprintf("txt-%d", from+i)}
			}
			input = map[string]any{"texts": list}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &errModerationDown{check: "gateway.deadline_exceeded"}
		}
		dl := deadline.UTC()
		req := aigateway.CallRequest{Operation: op, ModelGroup: group, Input: input,
			Constraints: &aigateway.Constraints{DeadlineAt: &dl},
			Labels:      map[string]string{"caller_biz": "replicate", "plan": planNo, "kind": strings.ToLower(kind)}}
		call, err := s.gw.Submit(ctx, req, aigateway.SubmitOptions{
			IdempotencyKey: moderationKey(s.cfg.KeyPrefix, planNo, strings.ToLower(kind), sig, attempt, part),
			Wait:           min(remaining, 60*time.Second), RetryFailed: true})
		if err != nil {
			return nil, &errModerationDown{check: "gateway.submit", raw: trimRunes(err.Error(), 500)}
		}
		for call != nil && !call.IsTerminal() {
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return nil, &errModerationDown{check: "gateway.deadline_exceeded", callID: call.ID}
			}
			if call, err = s.gw.Get(ctx, call.ID, min(remaining, 25*time.Second)); err != nil {
				return nil, &errModerationDown{check: "gateway.get", raw: trimRunes(err.Error(), 500)}
			}
		}
		if call == nil {
			return nil, &errModerationDown{check: "gateway.empty_response"}
		}
		out.callIDs = append(out.callIDs, call.ID)
		if !call.Succeeded() || call.Result == nil {
			class := call.Status
			if call.Failure != nil && call.Failure.Class != "" {
				class = call.Failure.Class
			}
			return nil, &errModerationDown{check: "gateway.status." + class, callID: call.ID}
		}
		var res struct {
			Results []modItem `json:"results"`
		}
		if err := json.Unmarshal(call.Result.Raw, &res); err != nil || len(res.Results) != len(part) {
			return nil, &errModerationDown{check: "gateway.result_count_mismatch", callID: call.ID, raw: trimRunes(string(call.Result.Raw), 2000)}
		}
		for _, it := range res.Results {
			switch strings.ToLower(it.Verdict) {
			case "pass", "review", "block":
			default:
				return nil, &errModerationDown{check: "gateway.verdict_unknown", callID: call.ID, raw: trimRunes(string(call.Result.Raw), 2000)}
			}
		}
		out.items = append(out.items, res.Results...)
		raws = append(raws, string(call.Result.Raw))
	}
	out.raw = strings.Join(raws, ",")
	return out, nil
}

// modOutcome 一类的策略评估结果。
type modOutcome struct {
	BizType  string   `json:"bizType"`
	Provider string   `json:"provider"`
	Status   string   `json:"status"` // PASSED / REJECTED / ERROR
	Hits     []string `json:"hits,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	CallIDs  []string `json:"callIds,omitempty"`
	Check    string   `json:"check,omitempty"` // 故障判据
	Count    int      `json:"count"`
}

// evaluate block 恒违规；review 按 ModerationReviewPass；hit(i) 给出第 i 条的命中目标描述。
func (s *Service) evaluateMod(b *modBatch, hit func(i int) string) (rejected bool, hits, labels []string, scene string) {
	for i, it := range b.items {
		violated := false
		switch strings.ToLower(it.Verdict) {
		case "block":
			violated = true
		case "review":
			violated = !s.cfg.ModerationReviewPass
		}
		if !violated {
			continue
		}
		hits = append(hits, hit(i))
		for _, l := range it.Labels {
			d := l.Label
			if d == "" {
				d = "other"
			}
			if l.SubLabel != "" {
				d += "/" + l.SubLabel
			}
			if l.VendorLabel != "" {
				d += "(" + l.VendorLabel + ")"
			}
			if l.Score != nil {
				d += fmt.Sprintf("=%g", *l.Score)
			}
			labels = appendOnce(labels, d)
			if scene == "" && l.Label != "" {
				scene = l.Label
			}
		}
	}
	return len(hits) > 0, hits, labels, scene
}

func (s *Service) logMod(ctx context.Context, l ModerationLog) {
	if s.mod == nil || s.mod.logger == nil {
		return
	}
	if err := s.mod.logger.LogModeration(context.WithoutCancel(ctx), l); err != nil {
		logx.WithContext(ctx).Errorf("replicate: 审核日志写入失败（不影响审核结论） 判据=MOD_LOG_FAILED request=%s biz=%s: %v", l.RequestID, l.BizType, err)
	}
}

// modInputs 一版方案要审的东西。
type modInputs struct {
	texts     []string // 叠字 + 改法说明
	refRids   []string // 参考图（图号顺序）
	frameRids []string // 源视频关键帧
}

func (s *Service) collectModInputs(ctx context.Context, p *model.Plan) (modInputs, error) {
	var in modInputs
	var ann Annotations
	_ = json.Unmarshal(p.Annotations, &ann)
	for _, t := range ann.Texts {
		if v := strings.TrimSpace(t.Text); v != "" {
			in.texts = append(in.texts, v)
		}
	}
	keys := make([]string, 0, len(ann.Dims))
	for k := range ann.Dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := strings.TrimSpace(ann.Dims[k].Note); v != "" {
			in.texts = append(in.texts, v)
		}
	}
	var refs []RefImage
	_ = json.Unmarshal(p.Refs, &refs)
	for _, r := range refs {
		if r.Rid != "" {
			in.refRids = append(in.refRids, r.Rid)
		}
	}
	a, err := s.store.GetAnalysis(ctx, p.AnalysisID)
	if err != nil {
		return in, err
	}
	if a != nil {
		var st AnalysisState
		_ = json.Unmarshal(a.State, &st)
		for _, f := range st.Frames {
			if f.Rid != "" {
				in.frameRids = append(in.frameRids, f.Rid)
			}
		}
	}
	return in, nil
}

// moderatePlan 一版方案的审核结论（有就直接用；没有就当场审并落库）。
// 返回 (nil, nil) 表示未开启审核；审核服务故障且 fail-close 时返回 *errModerationDown（不落结论，下次重审）。
//
// 已有结论什么时候不能直接用（要重审，成功则覆盖）：策略签名变了（fail_open / review_as_block / 模型组）；
// 确认时遇到 ERROR_OPEN（上次是故障放行，确认这一刻必须真审一次）；REJECTED 超过 ModerationRejectTTL（默认 24 小时，
// 供应商误判 / 词库调整后允许重审）。
func (s *Service) moderatePlan(ctx context.Context, p *model.Plan, confirming bool) (*model.Moderation, error) {
	if !s.moderationEnabled() {
		return nil, nil
	}
	sig := s.moderationPolicySig()
	attempt := 0
	if m, err := s.mod.store.GetModeration(ctx, p.ID, p.PlanHash); err != nil {
		return nil, err
	} else if m != nil {
		if s.moderationReusable(m, sig, confirming) {
			return m, nil
		}
		attempt = m.Attempt + 1 // 作废重审：新一轮，网关幂等键随之变化
	}
	in, err := s.collectModInputs(ctx, p)
	if err != nil {
		return nil, err
	}
	reqID := fmt.Sprintf(modRequestIDFmt, p.PlanNo)
	var outcomes []modOutcome
	var mu sync.Mutex
	var down *errModerationDown
	rejectReason := ""
	reject := func(o modOutcome, reason string) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, o)
		// 原因优先级：文字 > 参考图 > 原视频（用户最好改的先说）
		if rejectReason == "" || reasonRank(reason) < reasonRank(rejectReason) {
			rejectReason = reason
		}
	}
	record := func(o modOutcome) { mu.Lock(); outcomes = append(outcomes, o); mu.Unlock() }
	fail := func(o modOutcome, e *errModerationDown) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, o)
		if down == nil {
			down = e
		}
	}

	// 1) 文字：关键词库先行（命中就不再调网关）
	if len(in.texts) > 0 && s.mod.words != nil {
		joined := strings.Join(in.texts, "\n")
		if r, werr := s.mod.words.Check(ctx, "replicate.plan", joined); werr != nil {
			logx.WithContext(ctx).Errorf("replicate: 关键词库读取失败（跳过关键词，继续网关审核） plan=%s: %v", p.PlanNo, werr)
		} else if !r.Passed {
			o := modOutcome{BizType: ModBizText, Provider: ModProviderKeyword, Status: ModResultRejected, Count: len(in.texts), Hits: []string{"keyword"}}
			reject(o, modTextWords)
			s.logMod(ctx, ModerationLog{UserID: p.UserID, BizType: ModBizText, Provider: ModProviderKeyword, ResultStatus: ModResultRejected,
				Scene: "keyword", Label: r.Label, RejectedTarget: summarizeText(joined), Reason: modTextWords,
				RequestContent: joined, RequestID: reqID})
			in.texts = nil
		}
	}

	var wg sync.WaitGroup
	run := func(bizType string, images bool, inputs []string, targets func(i int) string, reason, content string) {
		defer wg.Done()
		b, err := s.gatewayModerate(ctx, p.PlanNo, bizType, sig, attempt, images, inputs)
		var de *errModerationDown
		if errors.As(err, &de) {
			logx.WithContext(ctx).Errorf("replicate: 审核服务故障 判据=%s plan=%s biz=%s failOpen=%v call=%s", de.check, p.PlanNo, bizType, s.cfg.ModerationFailOpen, de.callID)
			fail(modOutcome{BizType: bizType, Provider: ModProviderGateway, Status: ModResultError, Check: de.check, Count: len(inputs),
				CallIDs: nonEmpty(de.callID)}, de)
			s.logMod(ctx, ModerationLog{UserID: p.UserID, BizType: bizType, Provider: ModProviderGateway, Passed: s.cfg.ModerationFailOpen,
				ResultStatus: ModResultError, Scene: "error", Label: de.check, Reason: "审核服务故障：" + de.check,
				RequestContent: content, RawResponse: de.raw, RequestID: reqID, OutTaskID: de.callID})
			return
		}
		if err != nil {
			fail(modOutcome{BizType: bizType, Provider: ModProviderGateway, Status: ModResultError, Check: "internal", Count: len(inputs)},
				&errModerationDown{check: "internal", raw: err.Error()})
			return
		}
		rejected, hits, labels, scene := s.evaluateMod(b, targets)
		o := modOutcome{BizType: bizType, Provider: ModProviderGateway, Count: len(inputs), Hits: hits, Labels: labels, CallIDs: b.callIDs}
		l := ModerationLog{UserID: p.UserID, BizType: bizType, Provider: ModProviderGateway, Passed: !rejected, Scene: scene,
			Label: strings.Join(labels, "; "), RequestContent: content, RawResponse: b.raw, RequestID: reqID, OutTaskID: strings.Join(b.callIDs, ", ")}
		if rejected {
			o.Status, l.ResultStatus, l.RejectedTarget, l.Reason = ModResultRejected, ModResultRejected, strings.Join(hits, ", "), reason
			reject(o, reason)
			logx.WithContext(ctx).Infof("replicate: 方案审核未通过 判据=MODERATION_REJECTED plan=%s biz=%s 命中=%s", p.PlanNo, bizType, strings.Join(hits, ","))
		} else {
			o.Status, l.ResultStatus = ModResultPassed, ModResultPassed
			record(o)
		}
		s.logMod(ctx, l)
	}
	if len(in.texts) > 0 {
		wg.Add(1)
		texts := in.texts
		go run(ModBizText, false, texts, func(i int) string { return fmt.Sprintf("#%d %s", i+1, summarizeText(texts[i])) },
			modTextWords, strings.Join(texts, "\n"))
	}
	images := append(append([]string{}, in.refRids...), in.frameRids...)
	if len(images) > 0 {
		var urls map[string]string
		var serr error
		if s.signer == nil {
			serr = errors.New("没装配签名")
		} else {
			urls, serr = s.signer.ResourceURLs(ctx, images, 30*time.Minute)
		}
		missing := serr != nil
		for _, r := range images {
			if urls[r] == "" {
				missing = true
			}
		}
		if missing {
			check := "sign.unavailable"
			logx.WithContext(ctx).Errorf("replicate: 审核取图地址失败 判据=%s plan=%s: %v", check, p.PlanNo, serr)
			de := &errModerationDown{check: check}
			fail(modOutcome{BizType: ModBizImage, Provider: ModProviderGateway, Status: ModResultError, Check: check, Count: len(images)}, de)
			s.logMod(ctx, ModerationLog{UserID: p.UserID, BizType: ModBizImage, Provider: ModProviderGateway, Passed: s.cfg.ModerationFailOpen,
				ResultStatus: ModResultError, Scene: "error", Label: check, Reason: "审核服务故障：" + check,
				RequestContent: strings.Join(images, ", "), RequestID: reqID})
		} else {
			if len(in.refRids) > 0 {
				wg.Add(1)
				list := make([]string, len(in.refRids))
				for i, r := range in.refRids {
					list[i] = urls[r]
				}
				go run(ModBizImage, true, list, func(i int) string { return fmt.Sprintf("参考图 #%d", i+1) }, modTextRef, strings.Join(in.refRids, ", "))
			}
			if len(in.frameRids) > 0 {
				wg.Add(1)
				list := make([]string, len(in.frameRids))
				for i, r := range in.frameRids {
					list[i] = urls[r]
				}
				go run(ModBizVideo, true, list, func(i int) string { return fmt.Sprintf("第 %d 帧", i+1) }, modTextFrame, strings.Join(in.frameRids, ", "))
			}
		}
	}
	wg.Wait()

	detail, _ := json.Marshal(map[string]any{"outcomes": outcomes, "texts": len(in.texts), "refs": len(in.refRids), "frames": len(in.frameRids)})
	m := &model.Moderation{UserID: p.UserID, PlanID: p.ID, PlanNo: p.PlanNo, PlanHash: p.PlanHash, PolicySig: sig, Attempt: attempt, Detail: detail}
	switch {
	case rejectReason != "":
		// 有一类明确违规：即使另一类故障也是不通过（违规是确定的）
		m.Status, m.Reason = model.ModerationRejected, rejectReason
	case down != nil && !s.cfg.ModerationFailOpen:
		return nil, down
	case down != nil:
		m.Status = model.ModerationErrorOpen
	default:
		m.Status = model.ModerationPassed
	}
	if err := s.mod.store.PutModeration(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// moderationPolicySig 审核策略签名：影响结论的配置变了，旧结论作废。
func (s *Service) moderationPolicySig() string {
	return fmt.Sprintf("fo=%v;rab=%v;tg=%s;ig=%s", s.cfg.ModerationFailOpen, !s.cfg.ModerationReviewPass,
		s.cfg.ModerationTextGroup, s.cfg.ModerationImageGroup)
}

// moderationReusable 已有结论能否直接用（见 moderatePlan 注释）。
func (s *Service) moderationReusable(m *model.Moderation, sig string, confirming bool) bool {
	if m.PolicySig != sig {
		return false
	}
	switch m.Status {
	case model.ModerationErrorOpen:
		return !confirming
	case model.ModerationRejected:
		return s.now().Sub(m.UpdatedAt) < s.cfg.ModerationRejectTTL
	}
	return true
}

func reasonRank(r string) int {
	switch r {
	case modTextWords:
		return 0
	case modTextRef:
		return 1
	}
	return 2
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func summarizeText(t string) string {
	t = strings.TrimSpace(t)
	if r := []rune(t); len(r) > 20 {
		return string(r[:20]) + "…"
	}
	return t
}

// moderationBudget 一次后台审核的总时限：两类各自的整体截止并行跑，再加取签名地址 / 写库的余量。
func (s *Service) moderationBudget() time.Duration {
	return max(s.cfg.ModerationTextDeadline, s.cfg.ModerationImageDeadline) + 15*time.Second
}

// startModeration 后台发起一版方案的审核（同进程同方案同口径单飞，已在跑就复用）。脱离请求上下文：用户断开 / 请求返回后照样审完落库。
// confirming 区分口径：确认时 ERROR_OPEN 不能复用（必须真审一次），与生成方案时的预审不是同一次。
func (s *Service) startModeration(ctx context.Context, p *model.Plan, confirming bool) *modFlight {
	key := fmt.Sprintf("%d|%s|%v", p.ID, p.PlanHash, confirming)
	s.mod.mu.Lock()
	if f, ok := s.mod.flights[key]; ok {
		s.mod.mu.Unlock()
		return f
	}
	if s.mod.flights == nil {
		s.mod.flights = map[string]*modFlight{}
	}
	f := &modFlight{done: make(chan struct{})}
	s.mod.flights[key] = f
	s.mod.bg.Add(1)
	s.mod.mu.Unlock()

	pc := *p
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.moderationBudget())
	go func() {
		defer s.mod.bg.Done()
		defer func() {
			if r := recover(); r != nil {
				f.m, f.err = nil, fmt.Errorf("审核 panic: %v", r)
				logx.WithContext(bctx).Errorf("replicate: 后台审核异常 判据=MODERATION_PANIC plan=%s: %v", pc.PlanNo, r)
			}
			cancel()
			s.mod.mu.Lock()
			delete(s.mod.flights, key)
			s.mod.mu.Unlock()
			close(f.done)
		}()
		f.m, f.err = s.moderatePlan(bctx, &pc, confirming)
		if f.err != nil {
			logx.WithContext(bctx).Infof("replicate: 后台审核没做成（确认时再审） plan=%s confirming=%v: %v", pc.PlanNo, confirming, f.err)
		}
	}()
	return f
}

// awaitModeration 取一版方案的审核结论：已有可复用的结论直接返回；否则后台发起（或复用进行中的）审核，最多等 wait。
// ready=false 表示等到时限还没有结论（审核仍在后台继续，结论落库后下次直接用）。未开启审核返回 (nil, nil, true)。
func (s *Service) awaitModeration(ctx context.Context, p *model.Plan, confirming bool, wait time.Duration) (m *model.Moderation, err error, ready bool) {
	if !s.moderationEnabled() {
		return nil, nil, true
	}
	if got, gerr := s.mod.store.GetModeration(ctx, p.ID, p.PlanHash); gerr == nil && got != nil && s.moderationReusable(got, s.moderationPolicySig(), confirming) {
		return got, nil, true
	}
	f := s.startModeration(ctx, p, confirming)
	if wait <= 0 {
		select {
		case <-f.done:
			return f.m, f.err, true
		default:
			return nil, nil, false
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-f.done:
		return f.m, f.err, true
	case <-t.C:
		return nil, nil, false
	case <-ctx.Done():
		return nil, ctx.Err(), false
	}
}

// waitModerationIdle 等后台审核全部收尾（测试用）。
func (s *Service) waitModerationIdle() {
	if s.mod != nil {
		s.mod.bg.Wait()
	}
}

// gateModeration 确认前的硬卡点：不通过 / 故障（fail-close）都不让确认；结论还没出（等了 ModerationConfirmWait）→ MODERATION_PENDING。
// 三种情况都还没建 job、不预留点数。
func (s *Service) gateModeration(ctx context.Context, p *model.Plan) error {
	m, err, ready := s.awaitModeration(ctx, p, true, s.cfg.ModerationConfirmWait)
	if !ready && err == nil {
		logx.WithContext(ctx).Infof("replicate: 确认拒收 判据=MODERATION_PENDING user=%d plan=%s 已等=%v（后台审核仍在进行）", p.UserID, p.PlanNo, s.cfg.ModerationConfirmWait)
		return xerr.NewReason(xerr.Conflict, ReasonModerationPending, modTextWait)
	}
	var de *errModerationDown
	if errors.As(err, &de) {
		return xerr.NewReason(xerr.ThirdPartyError, ReasonModerationUnavailable, modTextDown)
	}
	if err != nil {
		return err
	}
	if m != nil && m.Status == model.ModerationRejected {
		return xerr.NewReason(xerr.ParamError, ReasonModerationRejected, m.Reason)
	}
	return nil
}

// planModerationView 方案视图里的审核状态：后台发起预审，最多等 ModerationPlanWait（默认不等）；没结论显示 pending（失败不挡方案，确认时再审）。
func (s *Service) planModerationView(ctx context.Context, p *model.Plan) *ModerationView {
	if !s.moderationEnabled() {
		return moderationView(nil, false)
	}
	m, err, ready := s.awaitModeration(ctx, p, false, s.cfg.ModerationPlanWait)
	if !ready || err != nil {
		return moderationView(nil, true)
	}
	return moderationView(m, true)
}

// moderateRedoNote 单段精修备注的审核（Redo 占名额 / 预留之前同步跑，口径同方案里用户写的文字）：先过关键词库，再过网关 moderation.text
// （一次调用；幂等键按 job + 备注内容，同一条备注重放同一结论）。
// 不落 afv_replicate_moderation（那张表按 plan_id + plan_hash 存方案级结论，备注没有对应方案行），只写 afv_moderation_log 留痕
// （request_id = replicate:job<id>-redo）。
// 没开启审核（没装配 / ModerationOff）时跳过，与方案审核一致；故障按 ModerationFailOpen：默认 fail-close 拒绝（MODERATION_UNAVAILABLE，没扣点），
// 放行时记 ERROR 日志。违规 → MODERATION_REJECTED（不暴露命中词）。
func (s *Service) moderateRedoNote(ctx context.Context, userID, jobID int64, note string) error {
	if note == "" || !s.moderationEnabled() {
		return nil
	}
	reqID := fmt.Sprintf(modRequestIDFmt, fmt.Sprintf("job%d-redo", jobID))
	if s.mod.words != nil {
		if r, err := s.mod.words.Check(ctx, "replicate.redo_note", note); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 关键词库读取失败（跳过关键词，继续网关审核） 判据=MOD_WORDS_UNAVAILABLE job=%d: %v", jobID, err)
		} else if !r.Passed {
			logx.WithContext(ctx).Infof("replicate: 重做备注审核未通过 判据=MODERATION_REJECTED provider=keyword job=%d", jobID)
			s.logMod(ctx, ModerationLog{UserID: userID, BizType: ModBizText, Provider: ModProviderKeyword, ResultStatus: ModResultRejected,
				Scene: "keyword", Label: r.Label, RejectedTarget: summarizeText(note), Reason: modTextWords, RequestContent: note, RequestID: reqID})
			return xerr.NewReason(xerr.ParamError, ReasonModerationRejected, modTextWords)
		}
	}
	b, err := s.gatewayModerate(ctx, fmt.Sprintf("job%d-redo", jobID), ModBizText, s.moderationPolicySig(), 0, false, []string{note})
	var de *errModerationDown
	if errors.As(err, &de) {
		logx.WithContext(ctx).Errorf("replicate: 重做备注审核服务故障 判据=%s job=%d failOpen=%v call=%s", de.check, jobID, s.cfg.ModerationFailOpen, de.callID)
		s.logMod(ctx, ModerationLog{UserID: userID, BizType: ModBizText, Provider: ModProviderGateway, Passed: s.cfg.ModerationFailOpen,
			ResultStatus: ModResultError, Scene: "error", Label: de.check, Reason: "审核服务故障：" + de.check, RequestContent: note,
			RawResponse: de.raw, RequestID: reqID, OutTaskID: de.callID})
		if s.cfg.ModerationFailOpen {
			return nil
		}
		return xerr.NewReason(xerr.ThirdPartyError, ReasonModerationUnavailable, modTextDown)
	}
	if err != nil {
		return err
	}
	rejected, hits, labels, scene := s.evaluateMod(b, func(int) string { return "备注 " + summarizeText(note) })
	l := ModerationLog{UserID: userID, BizType: ModBizText, Provider: ModProviderGateway, Passed: !rejected, Scene: scene,
		Label: strings.Join(labels, "; "), RequestContent: note, RawResponse: b.raw, RequestID: reqID, OutTaskID: strings.Join(b.callIDs, ", ")}
	if rejected {
		l.ResultStatus, l.RejectedTarget, l.Reason = ModResultRejected, strings.Join(hits, ", "), modTextWords
		s.logMod(ctx, l)
		logx.WithContext(ctx).Infof("replicate: 重做备注审核未通过 判据=MODERATION_REJECTED provider=gateway job=%d 标签=%s", jobID, strings.Join(labels, ","))
		return xerr.NewReason(xerr.ParamError, ReasonModerationRejected, modTextWords)
	}
	l.ResultStatus = ModResultPassed
	s.logMod(ctx, l)
	return nil
}
