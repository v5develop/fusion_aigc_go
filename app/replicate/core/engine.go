package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/quote"
	"aigc-local-life/app/replicate/rules/segment"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 出片引擎：确认（逐段预留）→ 逐段出片（网关 video.generate）→ 成功确认扣除 / 失败退回 → 汇总。
//
// 钱的不变量（DESIGN §9.5「只为成功段付费」）：
//   - 每段一张预留单，请求号确定且**从不复用**：replicate.<盐>-<job>-<kk>（确认）、-c<n>（第 n 次继续）、-u<n>（第 n 次重做）。
//     盐是确认时生成、冻结在 JobInput.BillingSalt 的内部随机串（不出现在用户可见的任何接口里），外部幂等键无法与之碰撞；
//     冻结命中已有单时 BillingLedger 还会核对入口 / 模型组 / 点数（ErrHoldMismatch）。盐为空的旧 job 沿用 replicate-<job>-<kk>。
//     billing 冻结按请求号幂等，同一次预留的重放（崩溃续跑、并发点击）只会命中同一张单；退回过的单不会被重新当成「已预留」。
//   - 一段只在 DELIVERING → 确认扣除成功后才算成功；预留已失效（ErrChargeGone）则不交付、置失败。
//   - 段最终失败才退回；自动重试（#r1、#r2…，至多 AutoRetryMax 次）共用原预留单，不另扣、不先退。
//   - 退回 / 确认扣除一律脱离请求上下文（context.WithoutCancel + SettleTimeout）：客户端断开、轮询器停机都不会半途丢账；
//     失败的镜像记「结算待重试」（reason 前缀 confirm: / refund:），DueJobs 对任何状态的 job 补扫（带退避）。
//   - 步骤状态是真值，预留镜像由 settleCharges 按步骤状态收敛（幂等），崩溃在任何两步之间都能在下一轮补齐。
//
// 推进由 Poller 在持有 job 行租约时调用 Advance；同一 job 同一时刻只有一个推进者，步骤迁移再用 CAS 兜底。

// JobInput 冻结输入（afv_replicate_job.input），确认后不再读方案表。字段只增不改。
type JobInput struct {
	PlanNo      string        `json:"planNo"`
	PlanHash    string        `json:"planHash"`
	EntryCode   string        `json:"entryCode"`
	ModelGroup  string        `json:"modelGroup"`
	Resolution  string        `json:"resolution"`
	AspectRatio string        `json:"aspectRatio,omitempty"`
	Audio       string        `json:"audio"`
	SourceRid   string        `json:"sourceRid"`
	RefImages   []RefImage    `json:"refImages,omitempty"`
	Segments    []JobSegment  `json:"segments"`
	MaxCostCNY  string        `json:"maxCostCny,omitempty"`
	Texts       []TextOverlay `json:"texts,omitempty"` // 成片叠字（finish.go）
	// KeyPrefix 确认时冻结的幂等键环境前缀（步骤 idem_key、切段 / 合成的媒体幂等键都带它）。
	KeyPrefix string `json:"keyPrefix,omitempty"`
	// Title / SourceThumbRid 历史列表摘要（确认时从分析结果冻结：一句话概述、首帧）。
	Title          string `json:"title,omitempty"`
	SourceThumbRid string `json:"sourceThumbRid,omitempty"`
	// Dialect 方案渲染用的方言码（写手校验按它）；WriterModelGroup / WriterSystem / WriterVersion 写手（writer.go）确认时冻结，
	// WriterModelGroup 为空 = 这个 job 没有写手步骤。
	Dialect          string `json:"dialect,omitempty"`
	WriterModelGroup string `json:"writerModelGroup,omitempty"`
	WriterSystem     string `json:"writerSystem,omitempty"`
	WriterVersion    string `json:"writerVersion,omitempty"`
	// BillingSalt 预留请求号的内部随机盐（确认时生成，见 reqFirst）；空 = 旧 job，沿用无盐请求号。
	BillingSalt string `json:"billingSalt,omitempty"`
	// AutoContinue / RestPoints 先做第 1 段确认时冻结：当时是否勾了「以后直接全部做」、其余段合计应付点数。
	// 自动继续只在两者都冻结过且现算其余段价等于 RestPoints 时进行；价变则停在待继续等用户确认（不按新价冻结）。
	AutoContinue bool  `json:"autoContinue,omitempty"`
	RestPoints   int64 `json:"restPoints,omitempty"`
}

// JobSegment 冻结的一段。
type JobSegment struct {
	segment.Segment
	Prompt string `json:"prompt"`
	// WriterBrief 写手的用户消息（确认时冻结；没开写手为空）
	WriterBrief string `json:"writerBrief,omitempty"`
}

// 请求号与步骤键（全部只含网关幂等键允许的字符 [A-Za-z0-9._:/#@-]）。
// salt 为 job 的内部盐（JobInput.BillingSalt）：非空时请求号是 replicate.<salt>-<job>-…，外部幂等键猜不到、碰不上；空 = 旧 job 的无盐格式。
func reqBase(salt string, jobID int64) string {
	if salt == "" {
		return fmt.Sprintf("replicate-%d", jobID)
	}
	return fmt.Sprintf("replicate.%s-%d", salt, jobID)
}
func reqFirst(salt string, jobID int64, k int) string {
	return fmt.Sprintf("%s-%02d", reqBase(salt, jobID), k)
}
func reqRest(salt string, jobID int64, k, attempt int) string {
	return fmt.Sprintf("%s-%02d-c%d", reqBase(salt, jobID), k, attempt)
}
func reqRedo(salt string, jobID int64, k, n int) string {
	return fmt.Sprintf("%s-%02d-u%d", reqBase(salt, jobID), k, n)
}

// newBillingSalt 预留请求号的内部盐：12 位随机十六进制（只含网关幂等键允许的字符）。
func newBillingSalt() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(b[:])
}
func genKey(k int, tag string) string            { return fmt.Sprintf("seg/%02d/generate%s", k, tag) }
func idemKey(jobID int64, stepKey string) string { return fmt.Sprintf("rpl-%d:%s", jobID, stepKey) }

// jobKey 带环境前缀的幂等键（前缀确认时冻结在 JobInput.KeyPrefix）。
func jobKey(prefix string, jobID int64, stepKey string) string {
	return prefix + idemKey(jobID, stepKey)
}

// 自动重试步骤键：原键 + #r<n>（n 从 1 起，第 n 次自动重试）。
var retrySuffix = regexp.MustCompile(`#r(\d+)$`)

func isAutoRetry(stepKey string) bool { return retrySuffix.MatchString(stepKey) }

// retryIndex 第几次自动重试（不是重试步骤返回 0）。
func retryIndex(stepKey string) int {
	m := retrySuffix.FindStringSubmatch(stepKey)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// nextRetryKey 下一次自动重试的步骤键：seg/01/generate → #r1 → #r2；#u1 → #u1#r1 → #u1#r2。
func nextRetryKey(stepKey string) string {
	return retrySuffix.ReplaceAllString(stepKey, "") + fmt.Sprintf("#r%d", retryIndex(stepKey)+1)
}

// 重做预留没成功的步骤（不代表该段结果，segChain / 汇总 / 合成 / 视图都跳过）。
const (
	codeHoldFailed    = "HOLD_FAILED"
	codeHoldAbandoned = "HOLD_ABANDONED"
	codeNotSubmitted  = "NOT_SUBMITTED"
	codeNotContinued  = "NOT_CONTINUED"
)

func holdVoid(st *model.Step) bool {
	if st.Status != model.StepCanceled {
		return false
	}
	c := deref(st.ErrorCode)
	return c == codeHoldFailed || c == codeHoldAbandoned
}

// detached 结算 / 清理用的上下文：不随请求取消（客户端断开），自带时限。
func (s *Service) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SettleTimeout)
}

var errHoldAbandoned = xerr.NewReason(xerr.Conflict, "SUBMIT_TIMEOUT", "提交超时，已冻结的点数会自动退回。请重试")

// userHoldError 预留失败交给用户的错误：业务错误（点数不足等）原样；其余（含冻结结果未知）不说「没有扣点数」。
func userHoldError(err error) error {
	if _, _, ok := xerr.Parse(err); ok {
		return err
	}
	return xerr.New(xerr.ServerError, "没能提交，请稍后重试；如有冻结的点数会自动退回")
}

// ---------------------------------------------------------------------------------------------
// 确认
// ---------------------------------------------------------------------------------------------

// ConfirmReq POST /api/v1/replicate/plans/{no}/confirm。
type ConfirmReq struct {
	UserID         int64
	PlanNo         string
	PlanHash       string
	Stage          string // first / all；空 = 默认（多段先做第 1 段，勾过「以后直接全部做」则全部）
	IdempotencyKey string // 必填，同用户唯一
	ExpectedPoints int64  // 页面展示的本次预留点数；>0 时与重算结果不一致返回 409 价变
	// WorkspaceID 工作空间；0 = 回落用户个人空间（WithWorkspace）。Go 网关的登录态只带 userId，没有「当前空间」，接口层恒传 0
	WorkspaceID int64
}

// Confirm 确认：校验方案指纹与价格 → 建 job（提交中）→ 占方案 → 逐段预留 → 排队。
// 任一段预留失败：已预留的全部退回、job 置失败、方案回到可确认，返回用户口径错误（如「点数不足，还差 N 点」）。
func (s *Service) Confirm(ctx context.Context, req ConfirmReq) (*JobView, error) {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > 64 {
		return nil, xerr.New(xerr.ParamMissing, "缺少请求标识，请刷新页面后重试")
	}
	if j, err := s.store.GetJobByRequest(ctx, req.UserID, key); err != nil || j != nil {
		if err != nil {
			return nil, err
		}
		return s.replayView(ctx, req.UserID, j.ID)
	}
	p, err := s.store.GetPlanByNo(ctx, req.UserID, req.PlanNo)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这个方案")
	}
	if p.Status == model.PlanConfirmed && p.JobID != nil {
		return s.replayView(ctx, req.UserID, *p.JobID)
	}
	if p.Status == model.PlanStale {
		// 改标注后的旧方案：不可再确认（前端拉 supersededBy 指向的新方案）
		logx.WithContext(ctx).Infof("replicate: 确认拒收 判据=PLAN_STALE user=%d plan=%s（已被改标注取代）", req.UserID, p.PlanNo)
		return nil, errPlanStale()
	}
	if p.Status != model.PlanReady || s.now().After(p.ExpiresAt) {
		return nil, xerr.NewReason(xerr.Conflict, "PLAN_EXPIRED", "这个方案已过期。点「重新生成方案」，不用重新分析")
	}
	if req.PlanHash != p.PlanHash {
		return nil, xerr.NewReason(xerr.Conflict, "PLAN_STALE", "方案刚更新了，请再看一眼价格")
	}
	// 内容审核（P0-4）：不通过 / 审核服务故障（fail-close）都不让确认，此时还没建 job、没动钱
	if err := s.gateModeration(ctx, p); err != nil {
		return nil, err
	}
	in, err := s.jobInput(ctx, p)
	if err != nil {
		return nil, err
	}
	n := len(in.Segments)
	in.BillingSalt = newBillingSalt()
	pref, _ := s.store.GetPref(ctx, req.UserID)
	autoContinue := pref != nil && pref.AutoContinue
	stage := req.Stage
	if stage == "" {
		stage = quote.DefaultStage(n, autoContinue)
	}
	if stage != quote.StageFirst && stage != quote.StageAll {
		return nil, xerr.New(xerr.ParamError, "确认方式不对，请刷新页面后重试")
	}
	if n == 1 {
		stage = quote.StageAll
	}
	idx := quote.Indexes(stage, n)
	lines, total, err := s.quoteIndexes(ctx, req.UserID, in, idx)
	if err != nil {
		return nil, err
	}
	if err := s.gateConfirmThreshold(ctx, req, in, n, len(idx), total); err != nil {
		return nil, err
	}
	if req.ExpectedPoints > 0 && req.ExpectedPoints != total {
		return nil, priceChanged(req.ExpectedPoints, total)
	}
	staged := stage == quote.StageFirst
	if staged && autoContinue {
		// 「以后直接全部做」：把其余段应付点数与开关一起冻结，自动继续时按它比价（不一致就停在待继续等用户确认）
		_, rest, err := s.quoteIndexes(ctx, req.UserID, in, quote.Indexes(quote.StageRest, n))
		if err != nil {
			return nil, err
		}
		in.AutoContinue, in.RestPoints = true, rest
	}
	inJSON, _ := json.Marshal(in)
	ws := req.WorkspaceID
	if ws == 0 && s.workspace != nil {
		if id, werr := s.workspace(ctx, req.UserID); werr != nil {
			logx.WithContext(ctx).Errorf("replicate: 查个人空间失败，job 工作空间记 0 判据=WORKSPACE_LOOKUP_FAILED user=%d: %v", req.UserID, werr)
		} else {
			ws = id
		}
	}
	j := &model.Job{UserID: req.UserID, WorkspaceID: ws, RequestID: key, PlanID: p.ID, PlanNo: p.PlanNo,
		PlanHash: p.PlanHash, Status: model.JobSubmitting, Staged: staged, Input: inJSON, SegTotal: n}
	chargeStage := model.ChargeStageAll
	if staged {
		chargeStage = model.ChargeStageFirst
	}
	got, created, err := s.store.CreateJob(ctx, j, func(id int64) (string, []*model.Step, []*model.Charge) {
		var steps []*model.Step
		var charges []*model.Charge
		held := map[int]bool{}
		for _, k := range idx {
			held[k] = true
			charges = append(charges, &model.Charge{JobID: id, UserID: req.UserID, SegIndex: k, Stage: chargeStage,
				RequestID: reqFirst(in.BillingSalt, id, k), Seconds: lines[k], Points: 0, Status: model.ChargePending})
		}
		for _, sg := range in.Segments {
			st := &model.Step{JobID: id, StepKey: genKey(sg.Index, ""), Kind: model.StepKindGateway, SegIndex: sg.Index,
				Status: model.StepPending, IdemKey: jobKey(in.KeyPrefix, id, genKey(sg.Index, ""))}
			if held[sg.Index] {
				st.BillingRequestID = ptr(reqFirst(in.BillingSalt, id, sg.Index))
			}
			steps = append(steps, st)
			if in.WriterModelGroup != "" {
				steps = append(steps, &model.Step{JobID: id, StepKey: writeKey(sg.Index), Kind: model.StepKindWriter, SegIndex: sg.Index,
					Status: model.StepPending, IdemKey: jobKey(in.KeyPrefix, id, writeKey(sg.Index))})
			}
		}
		if staged {
			steps = append(steps, &model.Step{JobID: id, StepKey: model.GateKey, Kind: model.StepKindGate,
				Status: model.StepWaiting, IdemKey: jobKey(in.KeyPrefix, id, model.GateKey)})
		}
		return taskcenter.Ref("rpl", id), steps, charges
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return s.replayView(ctx, req.UserID, got.ID)
	}
	// 先占方案（同一方案只能有一个 job），再动钱
	ok, err := s.store.ConfirmPlan(ctx, p.ID, got.ID)
	if err != nil {
		s.abortSubmit(ctx, got, 0, "没能提交，没有扣点数。请重试")
		return nil, err
	}
	if !ok {
		s.abortSubmit(ctx, got, 0, "这个方案已经提交过了")
		if cur, _ := s.store.GetPlan(ctx, p.ID); cur != nil && cur.JobID != nil {
			return s.replayView(ctx, req.UserID, *cur.JobID)
		}
		return nil, xerr.NewReason(xerr.Conflict, "PLAN_STALE", "方案刚更新了，请再看一眼价格")
	}
	alive := func(c context.Context) bool { // 心跳：每冻结一段刷新 job，回收器按 updated_at 判断「提交中」是否卡死
		ok, err := s.store.TouchJob(c, got.ID, []int8{model.JobSubmitting})
		return err != nil || ok // 心跳写失败不当成被回收（回收以库为准，下一段再试）
	}
	if herr := s.holdAll(ctx, got, in, chargeStage, alive); herr != nil {
		s.abortSubmit(ctx, got, p.ID, "")
		return nil, userHoldError(herr)
	}
	ok, err = s.store.TransitJob(ctx, got.ID, []int8{model.JobSubmitting}, model.JobQueued, map[string]any{"next_run_at": nil})
	if err != nil {
		return nil, err
	}
	if !ok {
		logx.WithContext(ctx).Infof("replicate: 确认收尾时 job 已不在提交中（被回收）判据=SUBMIT_REAPED job=%d", got.ID)
	}
	return s.JobView(ctx, req.UserID, got.ID)
}

// ExistingConfirm 确认前查已有 job：同幂等键已建过 job，或方案已确认 → 返回该 job 视图（Replay=true）；否则 (nil, nil)。
// 接口层先调它，命中就直接返回、不走限频（重放不该被 429 挡住，也不计次）。
func (s *Service) ExistingConfirm(ctx context.Context, userID int64, planNo, key string) (*JobView, error) {
	if key = strings.TrimSpace(key); key != "" && len(key) <= 64 {
		if j, err := s.store.GetJobByRequest(ctx, userID, key); err != nil || j != nil {
			if err != nil {
				return nil, err
			}
			return s.replayView(ctx, userID, j.ID)
		}
	}
	p, err := s.store.GetPlanByNo(ctx, userID, planNo)
	if err != nil || p == nil {
		return nil, err
	}
	if p.Status == model.PlanConfirmed && p.JobID != nil {
		return s.replayView(ctx, userID, *p.JobID)
	}
	return nil, nil
}

// replayView 确认的幂等重放 / 方案已确认：返回已有 job，标记 Replay（接口层据此不计限频额度）。
func (s *Service) replayView(ctx context.Context, userID, jobID int64) (*JobView, error) {
	v, err := s.JobView(ctx, userID, jobID)
	if v != nil {
		v.Replay = true
	}
	return v, err
}

// abortSubmit 确认没走完：取消未开始的步骤、job 提交中 → 失败、方案回到可确认（planID=0 不动方案）。
// 一律脱离请求上下文执行（客户端断开也要收干净）；reason 为空时按镜像推断（有退点待重试就不说「没有扣点数」）。
func (s *Service) abortSubmit(ctx context.Context, j *model.Job, planID int64, reason string) {
	dctx, cancel := s.detached(ctx)
	defer cancel()
	if reason == "" {
		reason = "没能提交（点数预留失败），没有扣点数。请重试"
		if cs, err := s.store.ListCharges(dctx, j.ID); err == nil {
			for _, c := range cs {
				if c.Status == model.ChargeSettleFailed || c.Status == model.ChargePending {
					reason = "没能提交（点数预留失败）。冻结的点数正在退回，一般 5 分钟内到账。请重试"
				}
			}
		}
	}
	ok, err := s.store.TransitJob(dctx, j.ID, []int8{model.JobSubmitting}, model.JobFailed,
		map[string]any{"error_reason": reason, "finished_at": s.now(), "next_run_at": nil})
	if err != nil || !ok {
		logx.WithContext(ctx).Errorf("replicate: 提交失败收尾没写上 判据=ABORT_SUBMIT job=%d ok=%v: %v", j.ID, ok, err)
	}
	s.cancelUnstarted(dctx, j.ID, codeNotSubmitted)
	if planID > 0 {
		_, _ = s.store.ReleasePlan(dctx, planID, j.ID)
	}
}

// cancelUnstarted 取消还没开始的出片步骤与门（job 不会再推进它们）。
func (s *Service) cancelUnstarted(ctx context.Context, jobID int64, code string) {
	steps, err := s.store.ListSteps(ctx, jobID)
	if err != nil {
		return
	}
	for _, st := range steps {
		if st.Status == model.StepPending || st.Status == model.StepWaiting {
			_, _ = s.store.UpdateStep(ctx, st.ID, st.Status, map[string]any{"status": model.StepCanceled,
				"finished_at": s.now(), "error_code": code})
		}
	}
}

// holdAll 逐段预留本次（stage）的预留镜像；任一失败把本次已预留的全部退回并返回错误。
func (s *Service) holdAll(ctx context.Context, j *model.Job, in *JobInput, stage string, alive func(context.Context) bool) error {
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return err
	}
	var mine []*model.Charge
	for _, c := range charges {
		if c.Stage == stage && c.Status == model.ChargePending {
			mine = append(mine, c)
		}
	}
	return s.holdCharges(ctx, j, in, mine, alive)
}

// holdCharges 逐段冻结。请求路径调用：
//   - 冻结整体时限 ReapAfter/2（比回收阈值短，回收器不会在请求还活着时动手）；
//   - 每冻结一段调 alive 刷心跳；alive=false（已被回收）→ 退回本次已冻结的并返回 errHoldAbandoned；
//   - 镜像 待预留→已预留 的 CAS 没抢到（回收器已改过镜像）→ 刚冻结的这一张立刻退回；
//   - 冻结报错一律按「可能已冻结」处理：按请求号幂等退回一次（没有单才记预留失败），见 releaseUncertain。
func (s *Service) holdCharges(ctx context.Context, j *model.Job, in *JobInput, charges []*model.Charge, alive func(context.Context) bool) error {
	sort.Slice(charges, func(a, b int) bool { return charges[a].SegIndex < charges[b].SegIndex })
	hctx, cancel := context.WithTimeout(ctx, s.cfg.ReapAfter/2)
	defer cancel()
	var done []*model.Charge
	for i, c := range charges {
		bc := s.charge(j.UserID, c.RequestID, in.ModelGroup, in.Resolution, c.Seconds)
		pts, err := s.ledger.Quote(hctx, bc)
		if err != nil {
			logx.WithContext(ctx).Infof("replicate: 预留前报价失败 判据=QUOTE_FAILED job=%d 请求号=%s: %v", j.ID, c.RequestID, err)
			s.abortHolds(ctx, j, done, charges[i:], err, false)
			return err
		}
		held, err := s.ledger.Hold(hctx, bc)
		if err != nil {
			code := "HOLD_FAILED"
			if errors.Is(err, ErrHoldUnknown) {
				code = "HOLD_STATUS_UNKNOWN"
			}
			// 命中别处先占的单（ErrHoldMismatch）：那不是本段的单，不能按请求号退回它，只记预留失败
			uncertain := !errors.Is(err, ErrHoldMismatch)
			if !uncertain {
				code = "HOLD_REPLAY_MISMATCH"
			}
			logx.WithContext(ctx).Infof("replicate: 预留失败 判据=%s job=%d 请求号=%s: %v", code, j.ID, c.RequestID, err)
			s.abortHolds(ctx, j, done, charges[i:], err, uncertain)
			return err
		}
		to := model.ChargeFree
		set := map[string]any{"status": to, "points": pts}
		if held {
			to = model.ChargeHeld
			at := s.now() // 冻结已返回之后取时：≥ billing 冻结时刻，从它起算续期不会缩短到期时间
			set["status"], set["held_at"] = to, at
			c.HeldAt = &at
		}
		ok, uerr := s.store.UpdateCharge(ctx, c.ID, []int8{model.ChargePending}, set)
		c.Status, c.Points = to, pts
		done = append(done, c)
		if uerr != nil {
			// 镜像没写上：引擎只认「已预留 / 不计费」的镜像才出片，继续下去这一段会永远排队。按失败处理：退回本次已冻结的（含这一张）并中止
			logx.WithContext(ctx).Errorf("replicate: 写预留镜像失败，退回并中止 判据=MIRROR_WRITE_FAILED job=%d 请求号=%s: %v", j.ID, c.RequestID, uerr)
			s.abortHolds(ctx, j, done, charges[i+1:], uerr, false)
			return uerr
		} else if !ok {
			logx.WithContext(ctx).Infof("replicate: 镜像已被回收器改写，退回刚冻结的单 判据=HOLD_ABANDONED job=%d 请求号=%s", j.ID, c.RequestID)
			s.abortHolds(ctx, j, done, charges[i+1:], errHoldAbandoned, false)
			return errHoldAbandoned
		}
		if alive != nil && !alive(ctx) {
			logx.WithContext(ctx).Infof("replicate: 预留中途 job 已被回收 判据=HOLD_ABANDONED job=%d 已冻结=%d", j.ID, len(done))
			s.abortHolds(ctx, j, done, charges[i+1:], errHoldAbandoned, false)
			return errHoldAbandoned
		}
	}
	return nil
}

// abortHolds 冻结中途失败：已冻结的退回；出错的那一张（uncertain=true）按「可能已冻结」幂等退回；其余记预留失败。
func (s *Service) abortHolds(ctx context.Context, j *model.Job, done, rest []*model.Charge, cause error, uncertain bool) {
	dctx, cancel := s.detached(ctx)
	defer cancel()
	for _, d := range done {
		s.refundCharge(dctx, j.UserID, d, "提交失败退回")
	}
	for i, r := range rest {
		if i == 0 && uncertain {
			s.releaseUncertain(dctx, j, r, cause)
			continue
		}
		_, _ = s.store.UpdateCharge(dctx, r.ID, []int8{model.ChargePending}, map[string]any{
			"status": model.ChargeHoldFailed, "reason": trimRunes(cause.Error(), 250)})
		r.Status = model.ChargeHoldFailed
	}
}

// releaseUncertain 冻结报错的那一张：按请求号幂等退回（Refund 先查真实状态）。
// 有单且退回成功 → 已退回；没有单 → 预留失败（真的没扣）；退回出错 → 结算待重试（refund:，补扫重做）。
func (s *Service) releaseUncertain(ctx context.Context, j *model.Job, c *model.Charge, cause error) {
	refunded, err := s.ledger.Refund(ctx, j.UserID, c.RequestID, "预留结果未知退回")
	from := []int8{model.ChargePending}
	switch {
	case err != nil:
		logx.WithContext(ctx).Errorf("replicate: 冻结结果未知且退回失败 判据=HOLD_UNCERTAIN_REFUND_FAILED job=%d 请求号=%s: %v（原因 %v）",
			j.ID, c.RequestID, err, cause)
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeSettleFailed,
			"reason": trimRunes(model.SettleReasonRefund+"hold-uncertain: "+err.Error(), 250)})
		c.Status = model.ChargeSettleFailed
	case refunded:
		logx.WithContext(ctx).Infof("replicate: 冻结报错但单已冻结，已退回 判据=HOLD_UNCERTAIN_REFUNDED job=%d 请求号=%s", j.ID, c.RequestID)
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeRefunded,
			"reason": "预留结果未知退回", "settled_at": s.now()})
		c.Status = model.ChargeRefunded
	default:
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeHoldFailed,
			"reason": trimRunes(cause.Error(), 250)})
		c.Status = model.ChargeHoldFailed
	}
}

// refundCharge 退回一张预留单并写镜像：成功 → 已退回 / 不计费；失败 → 结算待重试（refund:，补扫重做，用户看到「退点正在处理中」）。
// 脱离调用方上下文执行（客户端断开 / 停机也做完）。
func (s *Service) refundCharge(ctx context.Context, userID int64, c *model.Charge, reason string) {
	ctx, cancel := s.detached(ctx)
	defer cancel()
	refunded, err := s.ledger.Refund(ctx, userID, c.RequestID, reason)
	from := []int8{model.ChargePending, model.ChargeHeld, model.ChargeSettleFailed}
	switch {
	case errors.Is(err, ErrChargeConfirmed):
		// 预留单已扣除（如并发交付已确认）：退不了，重试也没用。镜像收敛为已扣除，ERROR 告警人工核对，不再补扫
		logx.WithContext(ctx).Errorf("replicate: 要退的单已扣除，镜像收敛为已扣除，需人工核对 判据=REFUND_ON_CONFIRMED job=%d 请求号=%s 原因=%s", c.JobID, c.RequestID, reason)
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeConfirmed,
			"reason": trimRunes("要退时已扣除（"+reason+"），需人工核对", 250), "settled_at": s.now()})
		c.Status = model.ChargeConfirmed
	case err != nil:
		logx.WithContext(ctx).Errorf("replicate: 退回失败 判据=REFUND_FAILED job=%d 请求号=%s: %v", c.JobID, c.RequestID, err)
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeSettleFailed,
			"reason": trimRunes(model.SettleReasonRefund+err.Error(), 250)})
		c.Status = model.ChargeSettleFailed
	case refunded:
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeRefunded, "reason": reason, "settled_at": s.now()})
		c.Status = model.ChargeRefunded
	default:
		_, _ = s.store.UpdateCharge(ctx, c.ID, from, map[string]any{"status": model.ChargeFree, "reason": reason, "settled_at": s.now()})
		c.Status = model.ChargeFree
	}
}

// confirmCharge 确认扣除（脱离调用方上下文）。
func (s *Service) confirmCharge(ctx context.Context, userID int64, requestID string) error {
	ctx, cancel := s.detached(ctx)
	defer cancel()
	return s.ledger.Confirm(ctx, userID, requestID)
}

func (s *Service) jobInput(ctx context.Context, p *model.Plan) (*JobInput, error) {
	a, err := s.store.GetAnalysis(ctx, p.AnalysisID)
	if err != nil {
		return nil, err
	}
	if a == nil || a.SourceRid == nil {
		return nil, xerr.New(xerr.NotFound, "这个方案的原片已经不在了。需要重新上传原片")
	}
	var segs []PlanSegmentView
	var prompts []string
	var refs []RefImage
	var ann Annotations
	if json.Unmarshal(p.Segments, &segs) != nil || json.Unmarshal(p.Prompts, &prompts) != nil || len(segs) != len(prompts) || len(segs) == 0 {
		return nil, xerr.New(xerr.ServerError, "方案数据异常，请重新生成方案")
	}
	_ = json.Unmarshal(p.Refs, &refs)
	_ = json.Unmarshal(p.Annotations, &ann)
	in := &JobInput{PlanNo: p.PlanNo, PlanHash: p.PlanHash, EntryCode: s.cfg.EntryCode, ModelGroup: p.ModelGroup, Resolution: p.Resolution,
		AspectRatio: p.AspectRatio, Audio: ann.Audio, SourceRid: *a.SourceRid, RefImages: refs, MaxCostCNY: s.cfg.MaxCostCNY, Texts: ann.Texts,
		KeyPrefix: s.cfg.KeyPrefix}
	an := &AnalysisResult{}
	if r, err := s.ResolveAnalysis(a); err == nil { // 读取时按当前规则纠偏
		an = r
		in.Title = trimRunes(strings.TrimSpace(an.Summary), 60)
	}
	var st AnalysisState
	if json.Unmarshal(a.State, &st) == nil {
		if len(st.Frames) > 0 {
			in.SourceThumbRid = st.Frames[0].Rid
		} else {
			in.SourceThumbRid = st.SheetRid
		}
	}
	dl := DialectFor(p.ModelGroup)
	in.Dialect = dl.Code
	if g := strings.TrimSpace(s.cfg.WriterModelGroup); g != "" {
		in.WriterModelGroup, in.WriterSystem, in.WriterVersion = g, writerSystemPrompt, WriterPromptVersion
	}
	for i, sg := range segs {
		js := JobSegment{Segment: sg.Segment, Prompt: prompts[i]}
		if in.WriterModelGroup != "" {
			js.WriterBrief = writerBrief(dl, an, ann, len(refs), sg.Segment, len(segs), prompts[i])
		}
		in.Segments = append(in.Segments, js)
	}
	return in, nil
}

// quoteIndexes 重算这些段的价：返回 段号→秒数 与合计。
func (s *Service) quoteIndexes(ctx context.Context, userID int64, in *JobInput, idx []int) (map[int]int, int64, error) {
	secs := map[int]int{}
	var total int64
	for _, k := range idx {
		sg := in.Segments[k-1]
		pts, err := s.ledger.Quote(ctx, s.charge(userID, "", in.ModelGroup, in.Resolution, sg.GenDuration))
		if err != nil {
			return nil, 0, err
		}
		secs[k] = sg.GenDuration
		total += pts
	}
	return secs, total, nil
}

func priceChanged(old, now int64) error {
	return xerr.NewReason(xerr.Conflict, ReasonQuoteChanged, fmt.Sprintf("价格有变化：%d → %d 点。确认后按新价格生成", old, now))
}

// ReasonQuoteChanged 页面点数与后端现算不一致，409：确认 / 继续 / 重做，以及发送前二次确认超阈值时缺失或不一致，都用这一个码。
// POINTS_CHANGED 只留给后台退点（admin.ReasonPointsChanged）。
const ReasonQuoteChanged = "QUOTE_CHANGED"

// gateConfirmThreshold 发送前二次确认（后端部分）：阈值 ConfirmThresholdPoints>0 且方案总价（全部段合计，与本次确认的阶段无关）
// 超过阈值时，确认必须带 expectedPoints 且等于本次现算的预留点数（stageTotal：先做第 1 段 = 第 1 段价，全部做 = 合计），
// 否则 409 QUOTE_CHANGED（与未超阈值时的价变同一个码），不建 job、不动钱。阈值为 0（默认）不启用，沿用「带了才比对、不一致回 QUOTE_CHANGED」。
// stageSegs < segTotal（先做第 1 段）时另报一次全部段的价得到方案总价；全部做时就是 stageTotal。
func (s *Service) gateConfirmThreshold(ctx context.Context, req ConfirmReq, in *JobInput, segTotal, stageSegs int, stageTotal int64) error {
	th := s.cfg.ConfirmThresholdPoints
	if th <= 0 {
		return nil
	}
	planTotal := stageTotal
	if stageSegs < segTotal {
		_, all, err := s.quoteIndexes(ctx, req.UserID, in, quote.Indexes(quote.StageAll, segTotal))
		if err != nil {
			return err
		}
		planTotal = all
	}
	if planTotal <= th || req.ExpectedPoints == stageTotal {
		return nil
	}
	logx.WithContext(ctx).Infof("replicate: 确认拒收 判据=CONFIRM_THRESHOLD user=%d plan=%s 方案总价=%d 阈值=%d 本次=%d 页面=%d",
		req.UserID, req.PlanNo, planTotal, th, stageTotal, req.ExpectedPoints)
	if req.ExpectedPoints <= 0 {
		return xerr.NewReason(xerr.Conflict, ReasonQuoteChanged, fmt.Sprintf("这次要预留 %d 点，请确认点数后再提交", stageTotal))
	}
	return xerr.NewReason(xerr.Conflict, ReasonQuoteChanged, fmt.Sprintf("点数有变化：%d → %d 点，请再确认一次", req.ExpectedPoints, stageTotal))
}

// ---------------------------------------------------------------------------------------------
// 先出第 1 段 → 继续 / 不继续
// ---------------------------------------------------------------------------------------------

// ContinueReq POST /api/v1/replicate/jobs/{id}/continue。
type ContinueReq struct {
	UserID         int64
	JobID          int64
	PlanHash       string
	IdempotencyKey string
	ExpectedPoints int64 // 页面展示的其余段点数；>0 时与重算不一致返回 409 价变
}

// Continue 满意，继续做剩下的段：重算其余段价格 → 逐段预留（请求号 -c<n>）→ 放行 gate → 执行中。
func (s *Service) Continue(ctx context.Context, req ContinueReq) (*JobView, error) {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > 64 {
		return nil, xerr.New(xerr.ParamMissing, "缺少请求标识，请刷新页面后重试")
	}
	j, err := s.ownJob(ctx, req.UserID, req.JobID)
	if err != nil {
		return nil, err
	}
	if j.ContinueRequestID != nil {
		return s.JobView(ctx, req.UserID, j.ID) // 已继续过：重复点击返回同一结果
	}
	if req.PlanHash != j.PlanHash {
		return nil, xerr.NewReason(xerr.Conflict, "PLAN_STALE", "方案刚更新了，请再看一眼价格")
	}
	switch j.Status {
	case model.JobWaiting:
	case model.JobContinuing:
		return nil, xerr.NewReason(xerr.Conflict, "IN_PROGRESS", "正在提交…")
	default:
		return nil, xerr.NewReason(xerr.Conflict, "NOT_WAITING", "第 1 段还没做好，先等它做完或重做第 1 段")
	}
	return s.doContinue(ctx, j, key, req.ExpectedPoints, false)
}

// doContinue 预留其余段并放行。expected：用户确认过的其余段点数；strict=true（自动继续）时无论是否为 0 都必须与现算一致，
// 否则返回价变、不占继续、不冻结（job 停在待继续）；strict=false 时 expected>0 才比对（接口层页面点数）。
func (s *Service) doContinue(ctx context.Context, j *model.Job, key string, expected int64, strict bool) (*JobView, error) {
	var in JobInput
	if err := json.Unmarshal(j.Input, &in); err != nil {
		return nil, xerr.New(xerr.ServerError, "任务数据异常")
	}
	idx := quote.Indexes(quote.StageRest, len(in.Segments))
	_, total, err := s.quoteIndexes(ctx, j.UserID, &in, idx)
	if err != nil {
		return nil, err
	}
	if (strict || expected > 0) && expected != total {
		return nil, priceChanged(expected, total)
	}
	ok, err := s.store.ClaimContinue(ctx, j.ID, j.ContinueAttempt)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NewReason(xerr.Conflict, "IN_PROGRESS", "正在提交…")
	}
	attempt := j.ContinueAttempt + 1
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	stepReq := map[int64]string{}
	var charges []*model.Charge
	for _, k := range idx {
		rid := reqRest(in.BillingSalt, j.ID, k, attempt)
		charges = append(charges, &model.Charge{JobID: j.ID, UserID: j.UserID, SegIndex: k, Stage: model.ChargeStageRest,
			RequestID: rid, Seconds: in.Segments[k-1].GenDuration, Status: model.ChargePending})
		for _, st := range steps {
			if st.StepKey == genKey(k, "") {
				stepReq[st.ID] = rid
			}
		}
	}
	back := func() { // 回到待继续（脱离请求上下文，客户端断开也要回去）
		dctx, cancel := s.detached(ctx)
		defer cancel()
		_, _ = s.store.TransitJob(dctx, j.ID, []int8{model.JobContinuing}, model.JobWaiting, nil)
	}
	if err := s.store.AddCharges(ctx, charges, stepReq); err != nil {
		back()
		return nil, err
	}
	alive := func(c context.Context) bool {
		ok, err := s.store.TouchJob(c, j.ID, []int8{model.JobContinuing})
		return err != nil || ok
	}
	if herr := s.holdCharges(ctx, j, &in, charges, alive); herr != nil {
		back()
		return nil, userHoldError(herr)
	}
	if err := s.openGate(ctx, j.ID, steps); err != nil {
		return nil, err
	}
	if _, err := s.store.TransitJob(ctx, j.ID, []int8{model.JobContinuing}, model.JobRunning,
		map[string]any{"continue_request_id": key, "next_run_at": nil}); err != nil {
		return nil, err
	}
	return s.JobView(ctx, j.UserID, j.ID)
}

func (s *Service) openGate(ctx context.Context, jobID int64, steps []*model.Step) error {
	for _, st := range steps {
		if st.StepKey == model.GateKey && st.Status == model.StepWaiting {
			_, err := s.store.UpdateStep(ctx, st.ID, model.StepWaiting, map[string]any{"status": model.StepSucceeded, "finished_at": s.now()})
			return err
		}
	}
	return nil
}

// Stop 不继续了：以仅第 1 段收尾为成功，其余段从未预留、不收费。
func (s *Service) Stop(ctx context.Context, userID, jobID int64) (*JobView, error) {
	j, err := s.ownJob(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	if j.Status != model.JobWaiting {
		if j.StopReason != nil {
			return s.JobView(ctx, userID, jobID)
		}
		return nil, xerr.NewReason(xerr.Conflict, "NOT_WAITING", "这一步已经处理过了")
	}
	if err := s.finishFirstOnly(ctx, j, "user_stop"); err != nil {
		return nil, err
	}
	return s.JobView(ctx, userID, jobID)
}

// finishFirstOnly 待继续 → 成功（只交付第 1 段）。先 CAS job（与「继续」互斥），再收掉 gate 与其余段步骤。
func (s *Service) finishFirstOnly(ctx context.Context, j *model.Job, reason string) error {
	ok, err := s.store.TransitJob(ctx, j.ID, []int8{model.JobWaiting}, model.JobSucceeded, map[string]any{
		"stop_reason": reason, "seg_done": 1, "seg_failed": 0, "finished_at": s.now()})
	if err != nil || !ok {
		return err
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return err
	}
	for _, st := range steps {
		if (st.StepKey == model.GateKey && st.Status == model.StepWaiting) || (st.SegIndex > 1 && st.Status == model.StepPending) {
			_, _ = s.store.UpdateStep(ctx, st.ID, st.Status, map[string]any{"status": model.StepCanceled, "finished_at": s.now(),
				"error_code": codeNotContinued})
		}
	}
	return nil
}

// SetAutoContinue 「以后直接全部做」（账户级）。
func (s *Service) SetAutoContinue(ctx context.Context, userID int64, on bool) error {
	return s.store.SetAutoContinue(ctx, userID, on)
}

// ---------------------------------------------------------------------------------------------
// 单段重做
// ---------------------------------------------------------------------------------------------

// RedoReq POST /api/v1/replicate/jobs/{id}/segments/{k}/redo。
type RedoReq struct {
	UserID         int64
	JobID          int64
	SegIndex       int
	IdempotencyKey string
	ExpectedPoints int64
}

// Redo 失败段单独重做：新预留（-u<n>）+ 新步骤（#u<n>，自己再有一次 #r1 自动重试）；原失败段的预留早已退回，不再动。
func (s *Service) Redo(ctx context.Context, req RedoReq) (*JobView, error) {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > 64 {
		return nil, xerr.New(xerr.ParamMissing, "缺少请求标识，请刷新页面后重试")
	}
	j, err := s.ownJob(ctx, req.UserID, req.JobID)
	if err != nil {
		return nil, err
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range charges {
		if c.ClientKey != nil && *c.ClientKey == key {
			return s.JobView(ctx, req.UserID, j.ID) // 重复点击
		}
	}
	switch j.Status {
	case model.JobQueued, model.JobRunning, model.JobFailed, model.JobPartial:
	default:
		return nil, xerr.NewReason(xerr.Conflict, "NOT_REDOABLE", "这个任务现在不能重做")
	}
	var in JobInput
	if err := json.Unmarshal(j.Input, &in); err != nil || req.SegIndex < 1 || req.SegIndex > len(in.Segments) {
		return nil, xerr.New(xerr.ParamError, "没有这一段")
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	chain := segChain(steps, req.SegIndex) // 跳过预留没成功的重做步骤（不代表该段结果）
	if len(chain) == 0 {
		return nil, xerr.New(xerr.ParamError, "没有这一段")
	}
	last := chain[len(chain)-1]
	switch last.Status {
	case model.StepFailed:
	case model.StepSucceeded:
		return nil, xerr.NewReason(xerr.Conflict, "SEGMENT_OK", "这一段已经做好了")
	case model.StepCanceled:
		return nil, xerr.NewReason(xerr.Conflict, "NOT_REDOABLE", "这一段没有开始做，不能重做")
	default:
		return nil, xerr.NewReason(xerr.Conflict, "SEGMENT_BUSY", "这一段还在生成")
	}
	if done := redoCount(chain); done >= s.cfg.RedoMaxPerSegment {
		return nil, xerr.NewReason(xerr.Conflict, "REDO_LIMIT", fmt.Sprintf("这一段已经重做了 %d 次，换个模型或调整方案再试", done))
	}
	// 编号按全部重做步骤（含预留没成功的）：请求号 / 步骤键从不复用，否则撞唯一键、后续重做永远进不去
	n := redoCount(segChainAll(steps, req.SegIndex)) + 1
	sg := in.Segments[req.SegIndex-1]
	pts, err := s.ledger.Quote(ctx, s.charge(j.UserID, "", in.ModelGroup, in.Resolution, sg.GenDuration))
	if err != nil {
		return nil, err
	}
	if req.ExpectedPoints > 0 && req.ExpectedPoints != pts {
		return nil, priceChanged(req.ExpectedPoints, pts)
	}
	redoable := []int8{model.JobQueued, model.JobRunning, model.JobFailed, model.JobPartial}
	// 原子占名额：redo_count 自增与上限在同一条件里，并发重做不会超限
	claimed, err := s.store.ClaimRedo(ctx, j.ID, s.cfg.RedoMaxPerJob, redoable)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if cur, _ := s.store.GetJob(ctx, j.ID); cur != nil && cur.RedoCount >= s.cfg.RedoMaxPerJob {
			logx.WithContext(ctx).Infof("replicate: 重做被拒 判据=REDO_LIMIT_JOB job=%d redo_count=%d 上限=%d", j.ID, cur.RedoCount, s.cfg.RedoMaxPerJob)
			return nil, xerr.NewReason(xerr.Conflict, "REDO_LIMIT", "这个任务重做次数太多了，调整方案后再做一条吧")
		}
		return nil, xerr.NewReason(xerr.Conflict, "NOT_REDOABLE", "这个任务现在不能重做")
	}
	// giveBack 重做没提交成功：还回名额；原来是终态就把 job 从执行中还原（ClaimRedo 已把它拉回执行中）。
	// 库里还有未终态步骤时 FinishJob 不会成功，交给 rollup。
	giveBack := func(release bool) {
		dctx, cancel := s.detached(ctx)
		defer cancel()
		if release {
			if err := s.store.ReleaseRedo(dctx, j.ID); err != nil {
				logx.WithContext(ctx).Errorf("replicate: 还回重做名额失败 job=%d: %v", j.ID, err)
			}
		}
		if model.JobFinal(j.Status) {
			_, _ = s.store.FinishJob(dctx, j.ID, []int8{model.JobRunning}, j.Status, j.SegTotal, map[string]any{"finished_at": j.FinishedAt})
		}
	}
	rid := reqRedo(in.BillingSalt, j.ID, req.SegIndex, n)
	sk := genKey(req.SegIndex, fmt.Sprintf("#u%d", n))
	step := &model.Step{JobID: j.ID, StepKey: sk, Kind: model.StepKindGateway, SegIndex: req.SegIndex, Status: model.StepHolding,
		IdemKey: jobKey(in.KeyPrefix, j.ID, sk), BillingRequestID: ptr(rid)}
	c := &model.Charge{JobID: j.ID, UserID: j.UserID, SegIndex: req.SegIndex, Stage: model.ChargeStageRedo, RequestID: rid,
		ClientKey: ptr(key), Seconds: sg.GenDuration, Status: model.ChargePending}
	if _, created, err := s.store.AddRedo(ctx, step, c); err != nil {
		giveBack(true)
		return nil, xerr.NewReason(xerr.Conflict, "IN_PROGRESS", "正在提交…")
	} else if !created {
		giveBack(true)
		return s.JobView(ctx, req.UserID, j.ID)
	}
	if herr := s.holdCharges(ctx, j, &in, []*model.Charge{c}, nil); herr != nil {
		dctx, cancel := s.detached(ctx)
		mine, _ := s.store.UpdateStep(dctx, step.ID, model.StepHolding, map[string]any{"status": model.StepCanceled, "finished_at": s.now(),
			"error_code": codeHoldFailed})
		cancel()
		giveBack(mine) // 被轮询器先取消（HOLD_ABANDONED）时名额由它还
		return nil, userHoldError(herr)
	}
	ok, err := s.store.UpdateStep(ctx, step.ID, model.StepHolding, map[string]any{"status": model.StepPending})
	if err != nil {
		return nil, err
	}
	if !ok {
		// 预留期间被轮询器按 HOLD_ABANDONED 取消了：刚冻结的单立即退回
		logx.WithContext(ctx).Infof("replicate: 重做步骤已被回收 判据=HOLD_ABANDONED job=%d step=%s", j.ID, sk)
		s.refundCharge(ctx, j.UserID, c, "重做提交超时退回")
		return nil, errHoldAbandoned // 名额由取消它的轮询器还；job 由 rollup 收尾
	}
	if _, err := s.store.TransitJob(ctx, j.ID, redoable, model.JobRunning,
		map[string]any{"finished_at": nil, "next_run_at": nil}); err != nil {
		return nil, err
	}
	return s.JobView(ctx, req.UserID, j.ID)
}

func redoCount(chain []*model.Step) int {
	n := 0
	for _, st := range chain {
		if strings.Contains(st.StepKey, "#u") && !isAutoRetry(st.StepKey) {
			n++
		}
	}
	return n
}

// segChain 某段的出片步骤（按 id 升序：原始 → #r1 → #u1 → #u1#r1 …），最后一个代表该段当前状态。
// 跳过重做预留没成功的步骤（HOLD_FAILED / HOLD_ABANDONED）：它们不代表该段结果，否则会遮住真正的最后一次失败、挡住后续重做。
func segChain(steps []*model.Step, k int) []*model.Step {
	var out []*model.Step
	for _, st := range segChainAll(steps, k) {
		if !holdVoid(st) {
			out = append(out, st)
		}
	}
	return out
}

// segChainAll 同 segChain，但不跳过任何步骤（重做编号用：请求号 / 步骤键从不复用）。
func segChainAll(steps []*model.Step, k int) []*model.Step {
	var out []*model.Step
	for _, st := range steps {
		if st.Kind == model.StepKindGateway && st.SegIndex == k {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (s *Service) ownJob(ctx context.Context, userID, jobID int64) (*model.Job, error) {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil || j.UserID != userID {
		return nil, xerr.New(xerr.NotFound, "找不到这个任务")
	}
	return j, nil
}

// ---------------------------------------------------------------------------------------------
// 推进
// ---------------------------------------------------------------------------------------------

// Advance 推进一个 job（Poller 持有 job 行租约时调用）。推进完按最新状态补发站内通知（notify.go，失败只打日志）。
func (s *Service) Advance(ctx context.Context, jobID int64) error {
	err := s.advance(ctx, jobID)
	s.notifyJob(ctx, jobID)
	return err
}

func (s *Service) advance(ctx context.Context, jobID int64) error {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil || j == nil {
		return err
	}
	switch j.Status {
	case model.JobSubmitting:
		return s.reapSubmitting(ctx, j)
	case model.JobContinuing:
		return s.reapContinuing(ctx, j)
	case model.JobWaiting:
		if j.ContinueDeadline != nil && s.now().After(*j.ContinueDeadline) {
			if err := s.finishFirstOnly(ctx, j, "continue_expired"); err != nil {
				return err
			}
		}
		s.renewJob(ctx, j, nil)
		return s.settleJob(ctx, j)
	case model.JobQueued, model.JobRunning:
	default:
		// 终态：先回收超时的重做预留步骤、取消提交回收留下的孤儿步骤，再收敛预留镜像（崩溃 / 退点失败补偿）
		if steps, err := s.store.ListSteps(ctx, j.ID); err == nil {
			for _, st := range steps {
				if st.Kind == model.StepKindGateway && st.Status == model.StepHolding {
					s.reapHolding(ctx, j, st)
				}
			}
			s.renewJob(ctx, j, steps) // 终态上扣费待重试的单：补扫重试期间预留不能到期
		}
		if n, err := s.store.CancelOrphanSteps(ctx, j.ID); err == nil && n > 0 {
			logx.WithContext(ctx).Infof("replicate: 终态 job 上的孤儿排队步骤已取消 判据=ORPHAN_PENDING job=%d 条数=%d", j.ID, n)
		}
		if err := s.settleJob(ctx, j); err != nil {
			return err
		}
		return s.finalize(ctx, j.ID) // 成片合成补做 / 重试（钱已在段级结清）
	}
	var in JobInput
	if err := json.Unmarshal(j.Input, &in); err != nil {
		return err
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return err
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return err
	}
	s.renewHolds(ctx, j, steps, charges)
	gateOpen := true
	for _, st := range steps {
		if st.StepKey == model.GateKey {
			gateOpen = st.Status == model.StepSucceeded
		}
	}
	// 写手步骤（不占出片并发；未放行的段不写，避免用户不继续时白写）
	for _, st := range steps {
		if st.Kind == model.StepKindWriter && (st.Status == model.StepPending || st.Status == model.StepRunning) && (gateOpen || st.SegIndex <= 1) {
			s.advanceWriter(ctx, j, &in, st)
		}
	}
	applyWritten(&in, steps)
	inflight := 0
	for _, st := range steps {
		if st.Kind == model.StepKindGateway && (st.Status == model.StepRunning || st.Status == model.StepDelivering) {
			inflight++
		}
	}
	sort.Slice(steps, func(a, b int) bool {
		if steps[a].SegIndex != steps[b].SegIndex {
			return steps[a].SegIndex < steps[b].SegIndex
		}
		return steps[a].ID < steps[b].ID
	})
	for _, st := range steps {
		if st.Kind != model.StepKindGateway {
			continue
		}
		switch st.Status {
		case model.StepRunning:
			s.takeOver(ctx, j, st)
			s.pollStep(ctx, j, &in, st, charges)
		case model.StepDelivering:
			s.takeOver(ctx, j, st)
			s.deliver(ctx, j, st, charges)
		case model.StepPending:
			if !gateOpen && st.SegIndex > 1 {
				continue
			}
			if inflight >= s.cfg.MaxInFlight {
				continue
			}
			if w := writerOf(steps, st.SegIndex); w != nil && !model.StepFinal(w.Status) {
				continue // 等这一段的写手写完（失败会回落模板，不会卡住）
			}
			c := chargeOf(charges, st.BillingRequestID)
			if c != nil && c.Status == model.ChargePending {
				s.repairPendingMirror(ctx, j, st, c, charges) // 冻结了但镜像没写上 / 冻结中途崩溃：按真实状态修正
			}
			if c != nil && (c.Status == model.ChargeRefunded || c.Status == model.ChargeHoldFailed) {
				// 兜底：钱已不在（退回 / 预留失败）的排队步骤永远不会出片，置失败让 job 能收尾（可重做，没有扣点数）
				logx.WithContext(ctx).Errorf("replicate: 排队步骤的预留已不在 判据=BILLING_RELEASED job=%d step=%s 镜像状态=%d", j.ID, st.StepKey, c.Status)
				s.failStep(ctx, j, st, failure(FailBillingGone, "BILLING_RELEASED", false), charges)
				continue
			}
			if c == nil || (c.Status != model.ChargeHeld && c.Status != model.ChargeFree) {
				continue // 没有有效预留单的段绝不出片
			}
			if c.Status == model.ChargeHeld {
				// 领段前核真实预留状态：排队期间可能已到期被回收（Reclaimer），钱不在了就不出片
				rs, err := s.ledger.Status(ctx, j.UserID, c.RequestID)
				if err != nil {
					logx.WithContext(ctx).Infof("replicate: 领段前查预留状态失败，下一轮再领 job=%d step=%s: %v", j.ID, st.StepKey, err)
					continue
				}
				if rs != "" && rs != resHeld {
					logx.WithContext(ctx).Errorf("replicate: 预留已失效不出片 判据=BILLING_GONE_BEFORE_START job=%d step=%s 请求号=%s", j.ID, st.StepKey, c.RequestID)
					s.failStep(ctx, j, st, failure(FailBillingGone, "BILLING_PRECHECK", false), charges)
					continue
				}
			}
			ok, err := s.store.UpdateStep(ctx, st.ID, model.StepPending, map[string]any{"status": model.StepRunning,
				"attempt": st.Attempt + 1, "lease_owner": s.owner, "started_at": s.now()})
			if err != nil || !ok {
				continue
			}
			inflight++
			st.Status, st.LeaseOwner = model.StepRunning, ptr(s.owner)
			s.pollStep(ctx, j, &in, st, charges)
		case model.StepHolding:
			s.reapHolding(ctx, j, st)
		}
	}
	if err := s.settleJob(ctx, j); err != nil {
		return err
	}
	if err := s.rollup(ctx, j.ID); err != nil {
		return err
	}
	return s.finalize(ctx, j.ID) // 刚到终态（成功 / 部分成功）就合成整条成片
}

// reapHolding 用户重做的预留卡在半路（请求在预留期间崩溃 / 超时）：按最近一次心跳（updated_at）超时取消步骤，
// 由取消成功的一方还回重做名额（重做请求自己发现被取消时不再还，避免还两次）；镜像由 settleJob 退回。
func (s *Service) reapHolding(ctx context.Context, j *model.Job, st *model.Step) {
	if s.now().Sub(st.UpdatedAt) <= s.cfg.ReapAfter {
		return
	}
	ok, err := s.store.UpdateStep(ctx, st.ID, model.StepHolding, map[string]any{"status": model.StepCanceled,
		"error_code": codeHoldAbandoned, "finished_at": s.now()})
	if err != nil || !ok {
		return
	}
	logx.WithContext(ctx).Infof("replicate: 重做预留超时取消 判据=HOLD_ABANDONED job=%d step=%s", j.ID, st.StepKey)
	if err := s.store.ReleaseRedo(ctx, j.ID); err != nil {
		logx.WithContext(ctx).Errorf("replicate: 还回重做名额失败 job=%d: %v", j.ID, err)
	}
}

// repairPendingMirror 排队步骤的镜像还是「待预留」且超过回收阈值（冻结后写镜像失败 / 冻结中途崩溃）：按预留单真实状态修正。
//   - HELD → 已预留（照常出片）；
//   - 已退回 / 已回收 → 已退回（步骤随后按 BILLING_RELEASED 置失败）；
//   - 没有单 → 预留失败（绝不当成不计费放行）；其余（已扣除等）只告警不动。
func (s *Service) repairPendingMirror(ctx context.Context, j *model.Job, st *model.Step, c *model.Charge, charges []*model.Charge) {
	if s.now().Sub(c.UpdatedAt) <= s.cfg.ReapAfter || (j.Status != model.JobQueued && j.Status != model.JobRunning) {
		return
	}
	rs, err := s.ledger.Status(ctx, j.UserID, c.RequestID)
	if err != nil {
		return
	}
	set := map[string]any{}
	switch rs {
	case resHeld:
		var in JobInput
		_ = json.Unmarshal(j.Input, &in)
		pts, _ := s.ledger.Quote(ctx, s.charge(j.UserID, c.RequestID, in.ModelGroup, in.Resolution, c.Seconds))
		// held_at 取修正这一刻（晚于真实冻结时刻）：续期只会偏晚、不会缩短到期时间；待预留超过 5 分钟才修正，偏晚的量在续期余量内
		set = map[string]any{"status": model.ChargeHeld, "points": pts, "reason": "镜像修正：预留单是 HELD", "held_at": s.now()}
	case resRefunded, resExpired, resCancelled:
		set = map[string]any{"status": model.ChargeRefunded, "reason": "镜像修正：预留单是 " + rs, "settled_at": s.now()}
	case "":
		set = map[string]any{"status": model.ChargeHoldFailed, "reason": "镜像修正：没有预留单"}
	default:
		logx.WithContext(ctx).Errorf("replicate: 待预留镜像对应的预留单状态异常 判据=MIRROR_PENDING_ODD job=%d 请求号=%s 状态=%s", j.ID, c.RequestID, rs)
		return
	}
	if ok, _ := s.store.UpdateCharge(ctx, c.ID, []int8{model.ChargePending}, set); ok {
		logx.WithContext(ctx).Infof("replicate: 修正待预留镜像 判据=MIRROR_PENDING_REPAIRED job=%d step=%s 请求号=%s 预留单=%q", j.ID, st.StepKey, c.RequestID, rs)
		c.Status = set["status"].(int8)
	}
}

// takeOver 在途步骤的执行实例换了（重启 / 别的实例接手 job 租约）：先把 lease_owner 改成自己（诊断用；状态 CAS 不依赖它）。
func (s *Service) takeOver(ctx context.Context, j *model.Job, st *model.Step) {
	if deref(st.LeaseOwner) == s.owner {
		return
	}
	if ok, _ := s.store.UpdateStep(ctx, st.ID, st.Status, map[string]any{"lease_owner": s.owner}); ok {
		logx.WithContext(ctx).Infof("replicate: 接管在途步骤 job=%d step=%s 原实例=%s 新实例=%s", j.ID, st.StepKey, deref(st.LeaseOwner), s.owner)
		st.LeaseOwner = ptr(s.owner)
	}
}

// renewHolds 预留续期，每轮续一次（billing.Renew，只对 HELD 生效）：
//   - 已预留：冻结超过 RenewAfter、链上还有未终态步骤（排队 / 在途 / 待确认）的单；
//   - 扣费待重试（6 + reason 前缀 confirm:）：段已出片、确认扣除调用失败等重试，真实预留单仍是 HELD——不论 job 状态都续，
//     否则重试拖过到期会被 Reclaimer 回收，变成「出了片却扣不到」（BILLING_GONE，平台承担成本）。
//
// 起算点一律是 held_at（冻结成功写镜像的时刻，之后不再改），任何一次续期的新到期（现在 + 租约）都不早于原到期
// （冻结时刻 + HoldTTL）：RenewAfter ≥ HoldTTL − 租约（+1 分钟时钟余量）由 withDefaults 保证。
// 旧行没有 held_at：已预留回退 updated_at（已预留期间镜像没被改写过，updated_at 就是写镜像的时刻）；
// 扣费待重试的 updated_at 已被改写、推不出冻结时刻，不续（宁可不续，也不缩短到期时间）。
// 视频预留默认 200 分钟到期，长任务排队 + 出片可能超过它；到期被回收的单在领段 / 交付时会被识别为失效，不出片 / 不交付。
func (s *Service) renewHolds(ctx context.Context, j *model.Job, steps []*model.Step, charges []*model.Charge) {
	for _, c := range charges {
		pending := confirmPending(c)
		if c.Status != model.ChargeHeld && !pending {
			continue
		}
		at := c.HeldAt
		if at == nil {
			if pending {
				continue
			}
			at = &c.UpdatedAt
		}
		if s.now().Sub(*at) < s.cfg.RenewAfter {
			continue
		}
		if !pending {
			open := false
			for _, st := range steps {
				if st.BillingRequestID != nil && *st.BillingRequestID == c.RequestID && !model.StepFinal(st.Status) {
					open = true
				}
			}
			if !open {
				continue
			}
		}
		if err := s.ledger.Renew(ctx, j.UserID, c.RequestID); err != nil {
			n := s.renewFailures.Add(1)
			logx.WithContext(ctx).Errorf("replicate: 预留续期失败（下一轮再续） 判据=RENEW_FAILED 累计=%d job=%d 请求号=%s 扣费待重试=%v: %v", n, j.ID, c.RequestID, pending, err)
		}
	}
}

// renewJob 待继续 / 终态 job 的续期（在途 job 在 advance 主路径里续）：重做预留中的步骤、扣费待重试的单都可能挂在这些状态上。
func (s *Service) renewJob(ctx context.Context, j *model.Job, steps []*model.Step) {
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return
	}
	if steps == nil {
		if steps, err = s.store.ListSteps(ctx, j.ID); err != nil {
			return
		}
	}
	s.renewHolds(ctx, j, steps, charges)
}

// pollStep RUNNING 的出片步骤：没有调用号就（按幂等键）提交，有就查；到终态分流到交付 / 重试 / 失败。
func (s *Service) pollStep(ctx context.Context, j *model.Job, in *JobInput, st *model.Step, charges []*model.Charge) {
	if s.gw == nil {
		return
	}
	var call *aigateway.Call
	var err error
	if st.GatewayCallID == nil {
		var req aigateway.CallRequest
		req, err = s.generateRequest(ctx, j, in, st)
		if err != nil {
			if s.cutter == nil || permanentMedia(err) {
				logx.WithContext(ctx).Errorf("replicate: 切段失败 判据=CUT_FAILED job=%d step=%s: %v", j.ID, st.StepKey, err)
				s.failStep(ctx, j, st, failure(FailCut, "CUT", false), charges)
			}
			return // 暂时性：下一轮同键重切
		}
		call, err = s.gw.Submit(ctx, req, aigateway.SubmitOptions{IdempotencyKey: st.IdemKey, Wait: s.cfg.PollWait, TraceID: deref(j.TaskID)})
		if err != nil {
			if f, temporary := classifySubmit(err); !temporary {
				logx.WithContext(ctx).Errorf("replicate: 出片提交被拒 判据=%s job=%d step=%s: %v", f.Class, j.ID, st.StepKey, err)
				s.failStep(ctx, j, st, f, charges)
			}
			return // 暂时性：保持 RUNNING，下一轮同键重提
		}
		// 调用号立即落库：崩溃恢复先按它查，不再提交
		if _, err := s.store.UpdateStep(ctx, st.ID, model.StepRunning, map[string]any{"gateway_call_id": call.ID, "gateway_status": call.Status}); err != nil {
			return
		}
		st.GatewayCallID = ptr(call.ID)
	} else {
		call, err = s.gw.Get(ctx, *st.GatewayCallID, s.cfg.PollWait)
		if err != nil {
			return
		}
	}
	if !call.IsTerminal() {
		if deref(st.GatewayStatus) != call.Status {
			_, _ = s.store.UpdateStep(ctx, st.ID, model.StepRunning, map[string]any{"gateway_status": call.Status})
		}
		return
	}
	if call.Succeeded() && call.Result != nil && call.Result.Video != nil && call.Result.Video.Rid != "" {
		out := map[string]any{"videoRid": call.Result.Video.Rid, "durationMs": call.Result.Video.DurationMs}
		if c := call.Result.Cover; c != nil {
			out["coverRid"] = c.Rid
		}
		if lf := call.Result.LastFrame; lf != nil {
			out["lastFrameRid"] = lf.Rid
		}
		if call.Cost != nil {
			out["costCny"] = call.Cost.AmountCNY
		}
		b, _ := json.Marshal(out)
		ok, err := s.store.UpdateStep(ctx, st.ID, model.StepRunning, map[string]any{"status": model.StepDelivering,
			"gateway_status": call.Status, "output": json.RawMessage(b)})
		if err != nil || !ok {
			return
		}
		st.Status, st.Output = model.StepDelivering, b
		s.deliver(ctx, j, st, charges)
		return
	}
	f := classifyCall(call)
	if call.Succeeded() { // 成功却没有视频：按供应商失败处理
		f = failure(FailVendor, "NO_VIDEO", true)
	}
	logx.WithContext(ctx).Infof("replicate: 段失败 判据=%s class=%s job=%d step=%s call=%s", f.Code, f.Class, j.ID, st.StepKey, call.ID)
	if n := retryIndex(st.StepKey); f.Transient && n < s.cfg.AutoRetryMax {
		key := nextRetryKey(st.StepKey)
		retry := &model.Step{JobID: j.ID, StepKey: key, Kind: model.StepKindGateway, SegIndex: st.SegIndex,
			Status: model.StepPending, IdemKey: jobKey(in.KeyPrefix, j.ID, key), BillingRequestID: st.BillingRequestID, RetryOf: ptr(st.StepKey)}
		ok, err := s.store.AddRetryStep(ctx, st, s.failSet(f, call.Status), retry)
		switch {
		case err != nil:
			logx.WithContext(ctx).Errorf("replicate: 建自动重试步骤失败（下一轮再判） job=%d step=%s: %v", j.ID, st.StepKey, err)
		case !ok:
			logx.WithContext(ctx).Infof("replicate: 自动重试没抢到（步骤已不是 RUNNING） 判据=RETRY_CAS_LOST job=%d step=%s", j.ID, st.StepKey)
		default:
			logx.WithContext(ctx).Infof("replicate: 自动重试 第 %d/%d 次 job=%d step=%s → %s", n+1, s.cfg.AutoRetryMax, j.ID, st.StepKey, key)
		}
		return // 预留单不动，由重试步骤接着用
	}
	if f.Transient {
		logx.WithContext(ctx).Infof("replicate: 自动重试已用完 判据=RETRY_EXHAUSTED job=%d step=%s 次数=%d", j.ID, st.StepKey, retryIndex(st.StepKey))
	}
	s.failStep(ctx, j, st, f, charges)
}

func (s *Service) failSet(f Failure, gwStatus string) map[string]any {
	set := map[string]any{"status": model.StepFailed, "error_code": f.Code, "error_class": f.Class, "error_msg": f.Text, "finished_at": s.now()}
	if gwStatus != "" {
		set["gateway_status"] = gwStatus
	}
	return set
}

// failStep 段最终失败：先 CAS 步骤（终态唯一），再退回该段预留单（失败 → 结算待重试，补扫重做）。
func (s *Service) failStep(ctx context.Context, j *model.Job, st *model.Step, f Failure, charges []*model.Charge) {
	ok, err := s.store.UpdateStep(ctx, st.ID, st.Status, s.failSet(f, ""))
	if err != nil || !ok {
		return
	}
	st.Status = model.StepFailed
	if c := chargeOf(charges, st.BillingRequestID); c != nil && model.ChargeOpen(c.Status) {
		s.refundCharge(ctx, j.UserID, c, "视频做同款第 "+fmt.Sprint(st.SegIndex)+" 段失败退回")
	}
}

// deliver DELIVERING：确认扣除该段预留，成功才置 SUCCEEDED。
//
// ErrChargeGone（预留单已不是 HELD：到期被 Reclaimer 回收 / 已退回）：钱已经回到用户可用余额，不能再扣，也就不交付——
// 步骤置失败 BILLING_GONE（用户可重做，没有扣点数），镜像按真实状态幂等收敛（refundCharge 查到 REFUNDED/EXPIRED 记已退回）；
// 这一段的供应商成本由平台承担（日志带 costCny 供对账）。
func (s *Service) deliver(ctx context.Context, j *model.Job, st *model.Step, charges []*model.Charge) {
	c := chargeOf(charges, st.BillingRequestID)
	if c == nil {
		logx.WithContext(ctx).Errorf("replicate: 出片成功但找不到预留镜像 判据=CHARGE_MISSING job=%d step=%s", j.ID, st.StepKey)
		return
	}
	switch c.Status {
	case model.ChargeHeld, model.ChargeSettleFailed:
		err := s.confirmCharge(ctx, j.UserID, c.RequestID)
		if errors.Is(err, ErrChargeGone) {
			// 先核真实状态：并发的另一次确认已扣除时也可能报 Gone，那是成功，不能拒绝交付
			if rs, serr := s.ledger.Status(ctx, j.UserID, c.RequestID); serr != nil {
				logx.WithContext(ctx).Errorf("replicate: 确认报预留失效、查状态失败（下一轮再判） job=%d 请求号=%s: %v", j.ID, c.RequestID, serr)
				return
			} else if settledStatus(rs) {
				logx.WithContext(ctx).Infof("replicate: 确认报预留失效但单已扣除，按成功交付 判据=CONFIRM_RACE_ALREADY job=%d 请求号=%s", j.ID, c.RequestID)
				err = nil
			}
		}
		if errors.Is(err, ErrChargeGone) {
			var out struct {
				CostCNY string `json:"costCny"`
			}
			_ = json.Unmarshal(st.Output, &out)
			logx.WithContext(ctx).Errorf("replicate: 预留已失效不交付 判据=BILLING_GONE job=%d step=%s 请求号=%s 平台承担成本=%s: %v",
				j.ID, st.StepKey, c.RequestID, out.CostCNY, err)
			if ok, _ := s.store.UpdateStep(ctx, st.ID, model.StepDelivering, s.failSet(failure(FailBillingGone, "BILLING", false), "")); ok {
				st.Status = model.StepFailed
			}
			s.refundCharge(ctx, j.UserID, c, "预留已失效（到期释放）")
			return
		}
		if err != nil {
			logx.WithContext(ctx).Errorf("replicate: 确认扣除失败（下一轮重试） 判据=CONFIRM_FAILED job=%d 请求号=%s: %v", j.ID, c.RequestID, err)
			_, _ = s.store.UpdateCharge(ctx, c.ID, []int8{model.ChargeHeld, model.ChargeSettleFailed}, map[string]any{"status": model.ChargeSettleFailed,
				"reason": trimRunes(model.SettleReasonConfirm+err.Error(), 250)})
			c.Status, c.Reason = model.ChargeSettleFailed, ptr(model.SettleReasonConfirm)
			return
		}
		_, _ = s.store.UpdateCharge(ctx, c.ID, []int8{model.ChargeHeld, model.ChargeSettleFailed}, map[string]any{
			"status": model.ChargeConfirmed, "reason": "段成功扣除", "settled_at": s.now()})
		c.Status = model.ChargeConfirmed
	case model.ChargeConfirmed, model.ChargeFree:
	default:
		// 已退回 / 预留失败：钱不在了，不交付
		logx.WithContext(ctx).Errorf("replicate: 镜像已不可扣不交付 判据=BILLING_GONE job=%d step=%s 镜像状态=%d", j.ID, st.StepKey, c.Status)
		if ok, _ := s.store.UpdateStep(ctx, st.ID, model.StepDelivering, s.failSet(failure(FailBillingGone, "BILLING", false), "")); ok {
			st.Status = model.StepFailed
		}
		return
	}
	if ok, _ := s.store.UpdateStep(ctx, st.ID, model.StepDelivering, map[string]any{"status": model.StepSucceeded, "finished_at": s.now()}); ok {
		st.Status = model.StepSucceeded
	}
}

func chargeOf(charges []*model.Charge, req *string) *model.Charge {
	if req == nil {
		return nil
	}
	for _, c := range charges {
		if c.RequestID == *req {
			return c
		}
	}
	return nil
}

// settleJob 预留镜像按步骤状态收敛（幂等，任何崩溃点之后都能补齐）：
//   - 链上有成功步骤 → 扣除（deliver 已做则跳过）；
//   - 链上全部终态且无成功 → 退回；终态 job 上还没开始的步骤（PENDING / HOLDING）不会再跑，按终态算；
//   - 待预留且超过回收阈值（不是正在进行的重做 / 继续）→ 退回（billing 没有单则记不计费）。
func (s *Service) settleJob(ctx context.Context, j *model.Job) error {
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return err
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return err
	}
	final := model.JobFinal(j.Status)
	for _, c := range charges {
		if !model.ChargeOpen(c.Status) {
			continue
		}
		if c.Status == model.ChargeSettleFailed && strings.HasPrefix(deref(c.Reason), model.SettleReasonRefund) {
			// 已经判定要退、只是退回调用失败：按镜像补退，不看步骤关联 / job 状态（继续中途失败的单早已不挂在步骤上）
			s.refundCharge(ctx, j.UserID, c, "退点补扫")
			continue
		}
		var chain []*model.Step
		for _, st := range steps {
			if st.BillingRequestID != nil && *st.BillingRequestID == c.RequestID {
				chain = append(chain, st)
			}
		}
		succeeded, active := false, false
		for _, st := range chain {
			switch st.Status {
			case model.StepSucceeded:
				succeeded = true
			case model.StepFailed, model.StepCanceled:
			default:
				// 排队 / 重做预留中 / 在途 / 待确认一律算在途：终态 job 上的重做（ClaimRedo 已把它拉回执行中，但本轮可能读到旧状态）不得误退；
				// 提交回收留下的孤儿 PENDING 步骤由 CancelOrphanSteps 先取消
				active = true
			}
		}
		switch {
		case c.Status == model.ChargePending:
			if s.now().Sub(c.UpdatedAt) > s.cfg.ReapAfter && j.Status != model.JobSubmitting && j.Status != model.JobContinuing && !active {
				s.refundCharge(ctx, j.UserID, c, "预留中断退回")
			}
		case succeeded:
			err := s.confirmCharge(ctx, j.UserID, c.RequestID)
			switch {
			case err == nil:
				_, _ = s.store.UpdateCharge(ctx, c.ID, []int8{model.ChargeHeld, model.ChargeSettleFailed}, map[string]any{
					"status": model.ChargeConfirmed, "reason": "对账补扣除", "settled_at": s.now()})
			default:
				logx.WithContext(ctx).Errorf("replicate: 对账补扣除失败 判据=CONFIRM_FAILED job=%d 请求号=%s: %v", j.ID, c.RequestID, err)
				_, _ = s.store.UpdateCharge(ctx, c.ID, []int8{model.ChargeHeld, model.ChargeSettleFailed}, map[string]any{
					"status": model.ChargeSettleFailed, "reason": trimRunes(model.SettleReasonConfirm+err.Error(), 250)})
			}
		case active:
		default:
			// 链上全部终态无成功（或这张单从未挂到任何步骤上，且 job 已不会再用它）→ 退回
			if len(chain) > 0 || final {
				s.refundCharge(ctx, j.UserID, c, "视频做同款段失败退回")
			}
		}
	}
	return nil
}

// rollup 按各段当前状态汇总 job：待继续 / 执行中 / 成功 / 部分成功 / 失败。
// 到终态用 FinishJob（库里再核一次「没有未终态的出片步骤」），与并发的用户重做互斥。
func (s *Service) rollup(ctx context.Context, jobID int64) error {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil || j == nil {
		return err
	}
	if j.Status != model.JobQueued && j.Status != model.JobRunning {
		return nil
	}
	steps, err := s.store.ListSteps(ctx, jobID)
	if err != nil {
		return err
	}
	gate := ""
	for _, st := range steps {
		if st.StepKey == model.GateKey {
			gate = st.Status
		}
	}
	started, active, done, failed, userCanceled := false, false, 0, 0, 0
	considered := 0
	for k := 1; k <= j.SegTotal; k++ {
		chain := segChain(steps, k)
		if len(chain) == 0 {
			continue
		}
		last := chain[len(chain)-1]
		if gate == model.StepWaiting && k > 1 {
			continue // 未放行的段不计入
		}
		considered++
		for _, st := range chain {
			if st.Status != model.StepPending && st.Status != model.StepHolding {
				started = true
			}
		}
		switch last.Status {
		case model.StepSucceeded:
			done++
		case model.StepFailed, model.StepCanceled:
			failed++
			if last.Status == model.StepCanceled && deref(last.ErrorCode) == codeCancelRest {
				userCanceled++
			}
		default:
			active = true
		}
	}
	set := map[string]any{"seg_done": done, "seg_failed": failed}
	if active {
		if j.Status == model.JobQueued && started {
			_, err = s.store.TransitJob(ctx, j.ID, []int8{model.JobQueued}, model.JobRunning, set)
		}
		return err
	}
	maxSeg := j.SegTotal
	if gate == model.StepWaiting {
		maxSeg = 1
	}
	if gate == model.StepWaiting && done == 1 {
		set["continue_deadline"] = s.now().Add(s.cfg.ContinueWindow)
		ok, err := s.store.FinishJob(ctx, j.ID, []int8{model.JobQueued, model.JobRunning}, model.JobWaiting, maxSeg, set)
		if err != nil || !ok {
			return err
		}
		// 「以后直接全部做」：第 1 段成功后自动继续（只在进入待继续时尝试一次；余额不够就停在待继续等用户）。
		// 只认确认时冻结的开关与其余段点数（JobInput.AutoContinue / RestPoints），且用户此刻仍开着；
		// 现算价与冻结价不一致 → 停在待继续等用户确认，绝不按新价冻结。
		var in JobInput
		_ = json.Unmarshal(j.Input, &in)
		if !in.AutoContinue {
			return nil
		}
		if pref, _ := s.store.GetPref(ctx, j.UserID); pref != nil && pref.AutoContinue {
			if cur, _ := s.store.GetJob(ctx, j.ID); cur != nil {
				if _, err := s.doContinue(ctx, cur, fmt.Sprintf("auto-%d", cur.ContinueAttempt+1), in.RestPoints, true); err != nil {
					code := "AUTO_CONTINUE_FAILED"
					if xerr.ReasonOf(err) == ReasonQuoteChanged {
						code = "AUTO_CONTINUE_PRICE_CHANGED"
					}
					logx.WithContext(ctx).Infof("replicate: 自动继续未成功，停在待继续 判据=%s job=%d 确认时其余段=%d 点: %v", code, j.ID, in.RestPoints, err)
				}
			}
		}
		return nil
	}
	to := model.JobFailed
	switch {
	case considered > 0 && done == considered:
		to = model.JobSucceeded
	case done > 0:
		to = model.JobPartial
	case considered > 0 && userCanceled == failed:
		to = model.JobCanceled // 全部段都是用户取消的（没开始就取消）：已取消，不是失败
	}
	set["finished_at"] = s.now()
	if to == model.JobCanceled {
		set["error_reason"] = "已取消，没开始的段已退回点数"
	} else if to == model.JobFailed {
		set["error_reason"] = "这次没有生成成功，没成功的段已退回点数"
	}
	ok, err := s.store.FinishJob(ctx, j.ID, []int8{model.JobQueued, model.JobRunning}, to, maxSeg, set)
	if err == nil && !ok {
		logx.WithContext(ctx).Infof("replicate: 汇总到终态没成功（有新的未终态步骤，如并发重做） 判据=FINISH_CAS_LOST job=%d", j.ID)
	}
	return err
}

// reapSubmitting 提交中超过回收阈值没有心跳（确认接口在逐段预留时崩溃 / 卡死）：
// 先 CAS job 提交中 → 失败（与仍活着的请求互斥：它的下一次心跳失败就会自己退回刚冻结的单），再取消步骤、逐张退回、方案回到可确认。
// 退回失败的镜像是「结算待重试」，崩溃在 CAS 与退回之间也一样：DueJobs 会继续捞「终态但有未结镜像」的 job 补退。
func (s *Service) reapSubmitting(ctx context.Context, j *model.Job) error {
	if s.now().Sub(j.UpdatedAt) <= s.cfg.ReapAfter {
		return nil
	}
	ok, err := s.store.TransitJob(ctx, j.ID, []int8{model.JobSubmitting}, model.JobFailed,
		map[string]any{"error_reason": "没能提交，冻结的点数会自动退回。请重试", "finished_at": s.now()})
	if err != nil || !ok {
		return err
	}
	logx.WithContext(ctx).Infof("replicate: 回收卡住的提交 判据=SUBMIT_STALE job=%d 心跳=%s", j.ID, j.UpdatedAt.Format(time.RFC3339))
	s.cancelUnstarted(ctx, j.ID, codeNotSubmitted)
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return err
	}
	pending := false
	for _, c := range charges {
		if model.ChargeOpen(c.Status) {
			s.refundCharge(ctx, j.UserID, c, "提交中断退回")
		}
		pending = pending || c.Status == model.ChargeSettleFailed
	}
	if !pending {
		_, _ = s.store.TransitJob(ctx, j.ID, []int8{model.JobFailed}, model.JobFailed, map[string]any{"error_reason": "没能提交，没有扣点数。请重试"})
	}
	_, _ = s.store.ReleasePlan(ctx, j.PlanID, j.ID)
	return nil
}

// reapContinuing 继续中超过回收阈值没有心跳：gate 已放行就往前走；否则回到待继续、退回本次继续的预留。
func (s *Service) reapContinuing(ctx context.Context, j *model.Job) error {
	if s.now().Sub(j.UpdatedAt) <= s.cfg.ReapAfter {
		return nil
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return err
	}
	for _, st := range steps {
		if st.StepKey == model.GateKey && st.Status == model.StepSucceeded {
			_, err := s.store.TransitJob(ctx, j.ID, []int8{model.JobContinuing}, model.JobRunning, nil)
			return err
		}
	}
	ok, err := s.store.TransitJob(ctx, j.ID, []int8{model.JobContinuing}, model.JobWaiting, nil)
	if err != nil || !ok {
		return err
	}
	logx.WithContext(ctx).Infof("replicate: 回收卡住的继续 判据=CONTINUE_STALE job=%d 第 %d 次继续", j.ID, j.ContinueAttempt)
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return err
	}
	suffix := fmt.Sprintf("-c%d", j.ContinueAttempt)
	for _, c := range charges {
		if c.Stage == model.ChargeStageRest && strings.HasSuffix(c.RequestID, suffix) && model.ChargeOpen(c.Status) {
			s.refundCharge(ctx, j.UserID, c, "继续中断退回")
		}
	}
	return nil
}

// generateRequest 一段的 video.generate 请求（冻结输入 → 网关规范操作；网关不知道任何业务语义，业务只在 labels 里不透明携带）。
func (s *Service) generateRequest(ctx context.Context, j *model.Job, in *JobInput, st *model.Step) (aigateway.CallRequest, error) {
	sg := in.Segments[st.SegIndex-1]
	ref := in.SourceRid
	if len(in.Segments) > 1 {
		if s.cutter == nil {
			return aigateway.CallRequest{}, errors.New("区间切段未装配")
		}
		rid, err := s.cutter.Cut(ctx, in.SourceRid, sg.GenT0, sg.GenT1, jobKey(in.KeyPrefix, j.ID, fmt.Sprintf("seg/%02d/cut", sg.Index)))
		if err != nil {
			return aigateway.CallRequest{}, err
		}
		ref = rid
	}
	input := map[string]any{
		"prompt": sg.Prompt, "duration_s": sg.GenDuration, "resolution": in.Resolution,
		"reference_videos": []aigateway.MediaRef{{URI: aigateway.MRScheme + ref}},
		"generate_audio":   in.Audio == AudioRegenerate,
	}
	if in.AspectRatio != "" {
		input["aspect_ratio"] = in.AspectRatio
	}
	if len(in.RefImages) > 0 {
		imgs := make([]aigateway.MediaRef, len(in.RefImages))
		for i, r := range in.RefImages {
			imgs[i] = aigateway.MediaRef{URI: aigateway.MRScheme + r.Rid}
		}
		input["reference_images"] = imgs
	}
	deadline := s.now().Add(s.cfg.SegmentDeadline).UTC()
	req := aigateway.CallRequest{Operation: "video.generate", ModelGroup: in.ModelGroup, Input: input,
		Constraints: &aigateway.Constraints{DeadlineAt: &deadline},
		Labels:      map[string]string{"caller_biz": "replicate", "job": fmt.Sprint(j.ID), "seg": fmt.Sprint(sg.Index)}}
	if mc := strings.TrimSpace(in.MaxCostCNY); mc != "" {
		req.Limits = &aigateway.Limits{MaxCost: mc}
	}
	return req, nil
}

// Guard 推进前抢行租约（生产实现见 app/replicate/httpapi：pkg/lease.Set，表 afv_replicate_analysis / afv_replicate_job）。
// Acquire=false 表示别的实例正在推进这一行，本轮跳过；推进完成后 Release。
type Guard interface {
	Acquire(ctx context.Context, table string, id int64) bool
	Release(ctx context.Context, table string, id int64)
}

// Tick Poller 一轮：取到期的分析与 job，逐个推进（不抢租约，单实例 / 单测用；多实例用 TickGuarded）。
func (s *Service) Tick(ctx context.Context, limit int) { s.TickGuarded(ctx, limit, nil) }

// TickGuarded 同 Tick，但每一行推进前先经 g 抢行租约（g 为 nil 时不抢）。
// 每推进一行就写回 next_run_at（在途短间隔、其余按年龄退避），DueJobs / DueAnalyses 按它升序取，任何一行都不会被饿死。
func (s *Service) TickGuarded(ctx context.Context, limit int, g Guard) {
	if ids, err := s.store.DueAnalyses(ctx, limit); err == nil {
		for _, id := range ids {
			if g != nil && !g.Acquire(ctx, model.TableAnalysis, id) {
				continue
			}
			delay := s.advanceAnalysisBurst(ctx, id)
			if err := s.store.RescheduleAnalysis(ctx, id, delay); err != nil {
				logx.WithContext(ctx).Errorf("replicate: 写分析 next_run_at 失败 analysis=%d: %v", id, err)
			}
			if g != nil {
				g.Release(ctx, model.TableAnalysis, id)
			}
		}
	}
	if ids, err := s.store.DueJobs(ctx, limit); err == nil {
		for _, id := range ids {
			if g != nil && !g.Acquire(ctx, model.TableJob, id) {
				continue
			}
			err := s.Advance(ctx, id)
			if err != nil {
				logx.WithContext(ctx).Errorf("replicate: 推进 job=%d 失败: %v", id, err)
			}
			if rerr := s.store.RescheduleJob(ctx, id, s.jobDelay(ctx, id, err)); rerr != nil {
				logx.WithContext(ctx).Errorf("replicate: 写 job next_run_at 失败 job=%d: %v", id, rerr)
			}
			if g != nil {
				g.Release(ctx, model.TableJob, id)
			}
		}
	}
}

// 退避：非在途（终态补退点 / 补合成、待继续里的补退点）按「结束多久了」拉长间隔，30 秒 ~ 30 分钟。
const (
	minBackoff = 30 * time.Second
	maxBackoff = 30 * time.Minute
	errBackoff = 30 * time.Second
	// 有未结镜像时的补扫间隔：15 秒 ~ 2 分钟
	chargeMinBackoff = 15 * time.Second
	chargeMaxBackoff = 2 * time.Minute
	tempBackoff      = 15 * time.Second
)

func backoff(age time.Duration) time.Duration {
	return min(max(age/10, minBackoff), maxBackoff)
}

// jobDelay 这一轮推进后多久再推。
func (s *Service) jobDelay(ctx context.Context, id int64, advErr error) time.Duration {
	if advErr != nil {
		return errBackoff
	}
	j, err := s.store.GetJob(ctx, id)
	if err != nil || j == nil {
		return errBackoff
	}
	switch j.Status {
	case model.JobQueued, model.JobRunning, model.JobSubmitting, model.JobContinuing:
		return s.cfg.ActiveRepoll
	}
	// 还有未结镜像（退点 / 扣除待重试、待预留、预留中）：按最老那张镜像的 updated_at 退避，上限 2 分钟（钱的事不能等半小时）
	if charges, err := s.store.ListCharges(ctx, id); err == nil {
		var oldest *time.Time
		for _, c := range charges {
			if model.ChargeOpen(c.Status) && (oldest == nil || c.UpdatedAt.Before(*oldest)) {
				t := c.UpdatedAt
				oldest = &t
			}
		}
		if oldest != nil {
			return min(max(s.now().Sub(*oldest)/10, chargeMinBackoff), chargeMaxBackoff)
		}
	}
	if j.Status == model.JobWaiting && j.ContinueDeadline != nil {
		if d := j.ContinueDeadline.Sub(s.now()); d > 0 {
			return min(d, backoff(s.now().Sub(j.UpdatedAt)))
		}
	}
	since := j.UpdatedAt
	if j.FinishedAt != nil {
		since = *j.FinishedAt
	}
	return backoff(s.now().Sub(since))
}

// advanceAnalysisBurst 推进一条分析：同步阶段一口气推完，遇到网关在途（台词 / 画面理解）就停，等下一轮。返回下次推进间隔。
func (s *Service) advanceAnalysisBurst(ctx context.Context, id int64) time.Duration {
	for i := 0; i < len(model.AnalysisStages); i++ {
		more, temp, err := s.advanceAnalysis(ctx, id)
		if err != nil {
			return errBackoff
		}
		if temp {
			return tempBackoff
		}
		if !more {
			return s.cfg.ActiveRepoll
		}
		if a, _ := s.store.GetAnalysis(ctx, id); a == nil || a.Stage == model.StageTranscribe || a.Stage == model.StageVision {
			return s.cfg.ActiveRepoll
		}
	}
	return s.cfg.ActiveRepoll
}
