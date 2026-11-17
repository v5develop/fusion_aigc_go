// Package admin 视频做同款后台（admin-api /api/v1/replicate/admin/**）的业务：任务检索、详情、按段人工退点、强制收尾。
//
// 权限（Go_V1.0.39，admin-api 中间件路由级覆盖）：检索 / 详情 replicate:view；退点 replicate:refund；强制收尾 replicate:force。
// 钱：人工退点只经 pkg/billing 的幂等接口——仍冻结（HELD）的单 billing.Refund，已扣除（CONFIRMED / PARTIALLY_REFUNDED）的单
// billing.RefundConfirmed（只退「消费 − 已补退」）；镜像只在计费真实状态已是退回 / 释放时才改已退回。原因必填。
// 审计：动作行 afv_replicate_admin_action（幂等键只认请求头 Idempotency-Key，没带时按 段 + 原因 + 当前预留单号集合 派生；
// 认领 + 令牌，过期可同键接管）+ 逐单明细 afv_replicate_admin_refund_line（动钱前先落 PENDING）。
// 「本次实际退了多少」以计费流水为准：退回原因带动作标记，流水描述以「本动作前缀」开头才计入，不同键并发退同一张单只记一次。
// 计费没开（Replicate.Billing.Mode≠on）时退点与强制收尾一律拒绝：否则镜像会被误判为「不计费」而真实预留单还冻着。
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 细分 reason。
const (
	ReasonPointsChanged   = "POINTS_CHANGED"
	ReasonNothingToRefund = "NOTHING_TO_REFUND"
	ReasonInProgress      = "IN_PROGRESS"
	ReasonBillingOff      = "BILLING_OFF"
	ReasonRefundBlocked   = "REFUND_BLOCKED"
)

// 预留单真实状态（afv_billing_reservation.status）。
const (
	resHeld              = "HELD"
	resConfirmed         = "CONFIRMED"
	resPartiallyRefunded = "PARTIALLY_REFUNDED"
)

// Billing 用到的 pkg/billing 方法（*billing.Service 直接满足；测试替换）。
type Billing interface {
	On() bool
	LoadReservation(ctx context.Context, userID int64, requestID string) (*billing.ReservationBrief, error)
	ConsumedPoints(ctx context.Context, b *billing.ReservationBrief) (consumed, refundedBefore int64, err error)
	Load(ctx context.Context, userID int64, requestID string) (*billing.Reservation, error)
	Refund(ctx context.Context, r *billing.Reservation, reason string) error
	RefundConfirmed(ctx context.Context, userID int64, requestID, reason string) (int64, error)
}

var _ Billing = (*billing.Service)(nil)

// Store 用到的业务表读写（*repo.Repo 直接满足）。
type Store interface {
	GetJob(ctx context.Context, id int64) (*model.Job, error)
	ListSteps(ctx context.Context, jobID int64) ([]*model.Step, error)
	ListCharges(ctx context.Context, jobID int64) ([]*model.Charge, error)
	UpdateCharge(ctx context.Context, id int64, from []int8, set map[string]any) (bool, error)
	AdminListJobs(ctx context.Context, f repo.JobFilter, offset, limit int) ([]*model.Job, int64, error)
	AdminGetPlan(ctx context.Context, id int64) (*model.Plan, error)
	ListModerations(ctx context.Context, planID int64) ([]*model.Moderation, error)
	ListModerationLogs(ctx context.Context, planNo string) ([]repo.ModerationLogRow, error)
	ListDeliveries(ctx context.Context, jobID int64) ([]*model.Delivery, error)
	ListAdminActions(ctx context.Context, jobID int64) ([]*model.AdminAction, error)
	GetAdminAction(ctx context.Context, jobID int64, action, key string) (*model.AdminAction, error)
	BeginAdminAction(ctx context.Context, a *model.AdminAction, ttl time.Duration) (*model.AdminAction, bool, error)
	FinishAdminAction(ctx context.Context, id int64, token string, status string, points int64, result []byte) (bool, error)
	TakeOverAdminAction(ctx context.Context, id int64, token string, ttl time.Duration) (bool, error)
	UpsertRefundLine(ctx context.Context, l *model.AdminRefundLine) (*model.AdminRefundLine, error)
	FinishRefundLine(ctx context.Context, id int64, token string, status string, points int64, errMsg string) (bool, error)
	ListRefundLines(ctx context.Context, actionID int64) ([]*model.AdminRefundLine, error)
	ListJobRefundLines(ctx context.Context, jobID int64) ([]*model.AdminRefundLine, error)
	// RefundLog 读计费流水（只读）：某幂等键的金额与描述（判断一笔退回是不是本动作写的）。
	RefundLog(ctx context.Context, userID int64, idemKey string) (amount int64, desc string, found bool, err error)
}

var _ Store = (*repo.Repo)(nil)

// Forcer 强制收尾（*core.Service 直接满足）。
type Forcer interface {
	ForceFinish(ctx context.Context, jobID int64) (*core.ForceFinishResult, error)
}

// Service 后台业务。
type Service struct {
	store  Store
	bill   Billing
	forcer Forcer
	// shareMedia 通过用户投稿时把成片复制进素材库桶（share.go；SetShareMedia 装配，可选）
	shareMedia ShareMedia
	// now 时钟（测试注入，nil = 系统时间）；rewardMonthlyLimit 每人每月奖励上限（0 = 默认 3）
	now                func() time.Time
	rewardMonthlyLimit int
}

// New 装配。
func New(store Store, bill Billing, forcer Forcer) *Service {
	return &Service{store: store, bill: bill, forcer: forcer}
}

// ---------------------------------------------------------------- 检索

// ListReq 检索条件。
type ListReq struct {
	UserID   int64
	Status   string // 对外码（succeeded / running …），逗号分隔可多选；空 = 全部
	From, To *time.Time
	JobID    int64
	TaskID   string
	PlanNo   string
	Page     int
	Size     int
}

// JobRow 列表一行。
type JobRow struct {
	JobID       int64      `json:"jobId"`
	TaskID      string     `json:"taskId"`
	PlanNo      string     `json:"planNo"`
	UserID      int64      `json:"userId"`
	Status      string     `json:"status"`
	StatusLabel string     `json:"statusLabel"`
	Title       string     `json:"title"`
	ModelGroup  string     `json:"modelGroup"`
	SegTotal    int        `json:"segTotal"`
	SegDone     int        `json:"segDone"`
	SegFailed   int        `json:"segFailed"`
	RedoCount   int        `json:"redoCount"`
	Error       string     `json:"error,omitempty"`
	StopReason  string     `json:"stopReason,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// ListResp 列表。
type ListResp struct {
	Items []JobRow `json:"items"`
	Total int64    `json:"total"`
	Page  int      `json:"page"`
	Size  int      `json:"size"`
}

func jobRow(j *model.Job) JobRow {
	var in core.JobInput
	_ = json.Unmarshal(j.Input, &in)
	code, label := core.JobStatusName(j.Status)
	title := in.Title
	if title == "" {
		title = "视频做同款"
	}
	return JobRow{JobID: j.ID, TaskID: deref(j.TaskID), PlanNo: j.PlanNo, UserID: j.UserID, Status: code, StatusLabel: label,
		Title: title, ModelGroup: in.ModelGroup, SegTotal: j.SegTotal, SegDone: j.SegDone, SegFailed: j.SegFailed,
		RedoCount: j.RedoCount, Error: deref(j.ErrorReason), StopReason: deref(j.StopReason), CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt}
}

// List 检索（id 倒序；size 默认 20、最大 100）。
func (s *Service) List(ctx context.Context, req ListReq) (*ListResp, error) {
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.Size <= 0 {
		req.Size = 20
	}
	req.Size = min(req.Size, 100)
	f := repo.JobFilter{UserID: req.UserID, From: req.From, To: req.To, JobID: req.JobID, TaskID: strings.TrimSpace(req.TaskID),
		PlanNo: strings.TrimSpace(req.PlanNo)}
	for _, c := range strings.Split(req.Status, ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		st, ok := core.JobStatusCode(c)
		if !ok {
			if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 8 {
				st, ok = int8(n), true
			}
		}
		if !ok {
			return nil, xerr.Param("status 不认识：" + c)
		}
		f.Statuses = append(f.Statuses, st)
	}
	jobs, total, err := s.store.AdminListJobs(ctx, f, (req.Page-1)*req.Size, req.Size)
	if err != nil {
		return nil, err
	}
	out := &ListResp{Items: make([]JobRow, 0, len(jobs)), Total: total, Page: req.Page, Size: req.Size}
	for _, j := range jobs {
		out.Items = append(out.Items, jobRow(j))
	}
	return out, nil
}

// ---------------------------------------------------------------- 详情

// StepRow 步骤时间线一行。
type StepRow struct {
	ID               int64      `json:"id"`
	StepKey          string     `json:"stepKey"`
	Kind             string     `json:"kind"`
	SegIndex         int        `json:"segIndex"`
	Status           string     `json:"status"`
	Attempt          int        `json:"attempt"`
	BillingRequestID string     `json:"billingRequestId,omitempty"`
	GatewayCallID    string     `json:"gatewayCallId,omitempty"`
	GatewayStatus    string     `json:"gatewayStatus,omitempty"`
	ErrorClass       string     `json:"errorClass,omitempty"`
	ErrorCode        string     `json:"errorCode,omitempty"`
	ErrorMsg         string     `json:"errorMsg,omitempty"`
	RetryOf          string     `json:"retryOf,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	StartedAt        *time.Time `json:"startedAt,omitempty"`
	FinishedAt       *time.Time `json:"finishedAt,omitempty"`
}

// ChargeRow 预留单镜像 + 计费真实状态。
type ChargeRow struct {
	ID            int64      `json:"id"`
	SegIndex      int        `json:"segIndex"`
	Stage         string     `json:"stage"`
	RequestID     string     `json:"requestId"`
	Seconds       int        `json:"seconds"`
	Points        int64      `json:"points"`
	MirrorStatus  int8       `json:"mirrorStatus"`
	MirrorLabel   string     `json:"mirrorLabel"`
	Reason        string     `json:"reason,omitempty"`
	BillingStatus string     `json:"billingStatus"` // afv_billing_reservation.status；没有单为空
	ReservationNo string     `json:"reservationNo,omitempty"`
	Mismatch      bool       `json:"mismatch"`         // 镜像与真实状态对不上（需人工核对）
	Refundable    int64      `json:"refundablePoints"` // 现在人工退能退多少
	BlockedReason string     `json:"blockedReason,omitempty"`
	SettledAt     *time.Time `json:"settledAt,omitempty"`
}

// SegmentRow 一段。
type SegmentRow struct {
	Index            int         `json:"index"`
	State            string      `json:"state"` // 链尾步骤状态
	GatewayCallIDs   []string    `json:"gatewayCallIds,omitempty"`
	FailReason       string      `json:"failReason,omitempty"`
	Steps            []StepRow   `json:"steps"`
	Charges          []ChargeRow `json:"charges"`
	RefundablePoints int64       `json:"refundablePoints"`
	CanRefund        bool        `json:"canRefund"`
}

// Detail 任务详情。
type Detail struct {
	Job          JobRow                   `json:"job"`
	Input        json.RawMessage          `json:"input"`
	Result       json.RawMessage          `json:"result,omitempty"`
	Segments     []SegmentRow             `json:"segments"`
	Steps        []StepRow                `json:"steps"` // 全部步骤（含 gate），按创建顺序
	Charges      []ChargeRow              `json:"charges"`
	Moderations  []*model.Moderation      `json:"moderations"`
	ModerationLg []repo.ModerationLogRow  `json:"moderationLogs"`
	Deliveries   []*model.Delivery        `json:"deliveries"`
	Actions      []*model.AdminAction     `json:"actions"`
	RefundLines  []*model.AdminRefundLine `json:"refundLines"` // 人工退点逐单明细（含处理中 / 失败的单）
	BillingOn    bool                     `json:"billingOn"`
	CanRefund    bool                     `json:"canRefund"`
	CanForce     bool                     `json:"canForce"`
	ForceBlocker string                   `json:"forceBlocker,omitempty"`
}

var mirrorLabels = map[int8]string{
	model.ChargePending: "待预留", model.ChargeHeld: "已预留", model.ChargeConfirmed: "已扣除", model.ChargeRefunded: "已退回",
	model.ChargeFree: "不计费", model.ChargeHoldFailed: "预留失败", model.ChargeSettleFailed: "结算待重试",
}

// mirrorMatches 镜像与真实状态是否一致（结算待重试 / 待预留属于过渡态，不算不一致）。
func mirrorMatches(mirror int8, real string) bool {
	switch mirror {
	case model.ChargeHeld:
		return real == resHeld
	case model.ChargeConfirmed:
		return real == resConfirmed || real == resPartiallyRefunded
	case model.ChargeRefunded:
		return real == "REFUNDED" || real == "EXPIRED" || real == "CANCELLED" || real == ""
	case model.ChargeFree, model.ChargeHoldFailed:
		return real == "" || real == "REFUNDED" || real == "EXPIRED" || real == "CANCELLED"
	}
	return true
}

func stepRow(st *model.Step) StepRow {
	return StepRow{ID: st.ID, StepKey: st.StepKey, Kind: st.Kind, SegIndex: st.SegIndex, Status: st.Status, Attempt: st.Attempt,
		BillingRequestID: deref(st.BillingRequestID), GatewayCallID: deref(st.GatewayCallID), GatewayStatus: deref(st.GatewayStatus),
		ErrorClass: deref(st.ErrorClass), ErrorCode: deref(st.ErrorCode), ErrorMsg: deref(st.ErrorMsg), RetryOf: deref(st.RetryOf),
		CreatedAt: st.CreatedAt, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt}
}

// activeRequests 还挂在未终态步骤上的请求号（这些 HELD 单不能人工退：段可能马上出片、要确认扣除）。
func activeRequests(steps []*model.Step) map[string]bool {
	out := map[string]bool{}
	for _, st := range steps {
		if st.BillingRequestID == nil || st.Kind != model.StepKindGateway {
			continue
		}
		if !model.StepFinal(st.Status) {
			out[*st.BillingRequestID] = true
		}
	}
	return out
}

// chargeRow 读真实状态并算可退点数。
func (s *Service) chargeRow(ctx context.Context, j *model.Job, c *model.Charge, active map[string]bool) (ChargeRow, error) {
	row := ChargeRow{ID: c.ID, SegIndex: c.SegIndex, Stage: c.Stage, RequestID: c.RequestID, Seconds: c.Seconds, Points: c.Points,
		MirrorStatus: c.Status, MirrorLabel: mirrorLabels[c.Status], Reason: deref(c.Reason), SettledAt: c.SettledAt}
	b, err := s.bill.LoadReservation(ctx, j.UserID, c.RequestID)
	if err != nil {
		return row, err
	}
	if b != nil {
		row.BillingStatus, row.ReservationNo = b.Status, b.No
	}
	row.Mismatch = !mirrorMatches(c.Status, row.BillingStatus)
	switch row.BillingStatus {
	case resHeld:
		switch {
		case j.Status == model.JobSubmitting || j.Status == model.JobContinuing:
			row.BlockedReason = "任务正在提交，稍后再试"
		case active[c.RequestID]:
			row.BlockedReason = "这一段还在排队 / 生成中，出片后会自动确认或退回"
		default:
			row.Refundable = b.PricePoints
		}
	case resConfirmed, resPartiallyRefunded:
		consumed, before, err := s.bill.ConsumedPoints(ctx, b)
		if err != nil {
			return row, err
		}
		row.Refundable = max(0, consumed-before)
	}
	return row, nil
}

// Detail 任务详情。
func (s *Service) Detail(ctx context.Context, jobID int64) (*Detail, error) {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, xerr.New(xerr.NotFound, "任务不存在")
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	d := &Detail{Job: jobRow(j), Input: j.Input, Result: j.ResultDocument, BillingOn: s.bill.On(), Steps: make([]StepRow, 0, len(steps))}
	for _, st := range steps {
		d.Steps = append(d.Steps, stepRow(st))
	}
	active := activeRequests(steps)
	bySeg := map[int][]ChargeRow{}
	for _, c := range charges {
		row, err := s.chargeRow(ctx, j, c, active)
		if err != nil {
			return nil, err
		}
		if !d.BillingOn {
			row.Refundable = 0
		}
		d.Charges = append(d.Charges, row)
		bySeg[c.SegIndex] = append(bySeg[c.SegIndex], row)
	}
	for k := 1; k <= j.SegTotal; k++ {
		seg := SegmentRow{Index: k, Charges: bySeg[k]}
		for _, st := range steps {
			if st.Kind != model.StepKindGateway || st.SegIndex != k {
				continue
			}
			r := stepRow(st)
			seg.Steps = append(seg.Steps, r)
			seg.State = st.Status
			if r.GatewayCallID != "" {
				seg.GatewayCallIDs = append(seg.GatewayCallIDs, r.GatewayCallID)
			}
			if st.Status == model.StepFailed {
				seg.FailReason = strings.TrimSpace(r.ErrorCode + " " + r.ErrorMsg)
			}
		}
		for _, c := range seg.Charges {
			seg.RefundablePoints += c.Refundable
		}
		seg.CanRefund = seg.RefundablePoints > 0
		d.CanRefund = d.CanRefund || seg.CanRefund
		d.Segments = append(d.Segments, seg)
	}
	if d.ForceBlocker = core.ForceBlocker(j, steps, charges); d.ForceBlocker == "" && !d.BillingOn {
		d.ForceBlocker = "后台计费未开启（ReplicateAdmin.Billing.Mode 须为 on），不能收尾动钱"
	}
	d.CanForce = d.ForceBlocker == ""
	if p, err := s.store.AdminGetPlan(ctx, j.PlanID); err == nil && p != nil {
		if d.Moderations, err = s.store.ListModerations(ctx, p.ID); err != nil {
			logx.WithContext(ctx).Errorf("replicate admin: 读审核结论失败 job=%d: %v", j.ID, err)
		}
	}
	if d.ModerationLg, err = s.store.ListModerationLogs(ctx, j.PlanNo); err != nil {
		logx.WithContext(ctx).Errorf("replicate admin: 读审核日志失败 job=%d: %v", j.ID, err)
	}
	if d.Deliveries, err = s.store.ListDeliveries(ctx, j.ID); err != nil {
		return nil, err
	}
	if d.Actions, err = s.store.ListAdminActions(ctx, j.ID); err != nil {
		return nil, err
	}
	if d.RefundLines, err = s.store.ListJobRefundLines(ctx, j.ID); err != nil {
		return nil, err
	}
	return d, nil
}

// ---------------------------------------------------------------- 人工退点

const (
	// actionLease 后台动作的认领时长：处理中崩溃后，过了这段时间同一幂等键可以接管。
	actionLease = 2 * time.Minute
	// workTimeout 动钱 / 收尾脱离请求上下文的总时限（管理员关页面也做完）。必须比认领短至少 30 秒，
	// 否则认领过期被接管时旧执行者可能还在动钱（结果写回带令牌，旧执行者写入会落空，但别让两边同时动钱）。
	workTimeout = 60 * time.Second
)

func init() {
	if actionLease <= workTimeout+30*time.Second {
		panic("replicate admin: actionLease 必须大于 workTimeout + 30s")
	}
}

// RefundReq POST /jobs/{id}/refund。
type RefundReq struct {
	JobID          int64
	AdminID        int64
	SegIndexes     []int
	Reason         string
	ExpectedPoints int64  // 页面展示的「现在还能退」合计（只算尚未退成的单）；与后端现算不一致 → 409 POINTS_CHANGED
	IdempotencyKey string // 只认请求头 Idempotency-Key；空 = 按 段 + 原因 + 当前预留单号集合 派生
}

// RefundResp 退点结果（lines 为本动作的全部逐单明细，含此前重试已退成的）。
type RefundResp struct {
	ActionID   int64                    `json:"actionId"`
	Points     int64                    `json:"points"`     // 本动作累计实际退回
	ThisPoints int64                    `json:"thisPoints"` // 这一次请求实际退回
	Lines      []*model.AdminRefundLine `json:"lines"`
	Replay     bool                     `json:"replay"`
	Status     string                   `json:"status"`
}

// PartialError 部分单没退成：错误码 5001，同时带回逐单明细（接口层把 Resp 放进 data 一起下发）。
type PartialError struct {
	Resp *RefundResp
	Msg  string
}

func (e *PartialError) Error() string { return e.Msg }

func deriveKey(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:40]
}

// 计费流水描述前缀（归属判断按前缀精确匹配）：
//   - billing.Refund 的流水描述 = 传入的原因（trim 到 512），我们传 heldReasonPrefix(id) + 原因；
//   - billing.RefundConfirmed 的流水描述 = "人工处理退回：" + 传入的原因（契约测试见 admin_contract_test.go）。
const confirmedDescPrefix = "人工处理退回："

func heldReasonPrefix(actionID int64) string {
	return fmt.Sprintf("后台人工退点[a%d]：", actionID)
}

// sanitizeReason 管理员填的原因里去掉 `[a` 片段，避免伪造别的动作的标记。
func sanitizeReason(r string) string { return strings.ReplaceAll(r, "[a", "") }

func (s *Service) replay(ctx context.Context, a *model.AdminAction) (*RefundResp, error) {
	lines, err := s.store.ListRefundLines(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return &RefundResp{ActionID: a.ID, Points: a.Points, Lines: lines, Replay: true, Status: a.Status}, nil
}

// ledgerOwned 读计费流水：本动作在这张单上退回了多少（流水描述以本动作前缀开头才算）；found = 这张单的退回流水是否存在（任何人写的）。
func (s *Service) ledgerOwned(ctx context.Context, userID int64, resNo string, actionID int64) (pts int64, found bool, err error) {
	prefix := heldReasonPrefix(actionID)
	for _, c := range []struct{ key, desc string }{
		{billing.RefundKeyPrefix + resNo, prefix},
		{billing.PostRefundKeyPrefix + resNo, confirmedDescPrefix + prefix},
	} {
		amt, desc, ok, err := s.store.RefundLog(ctx, userID, c.key)
		if err != nil {
			return 0, found, err
		}
		if ok {
			found = true
			if strings.HasPrefix(desc, c.desc) {
				return amt, true, nil
			}
		}
	}
	return 0, found, nil
}

func refundedLike(st string) bool { return st == "REFUNDED" || st == "EXPIRED" || st == "CANCELLED" }

// errStateChanged 预留单状态在读与退之间变成了没法处理的状态（如又回到 HELD 之外的未知态），明细记 FAILED。
var errStateChanged = fmt.Errorf("预留单状态已变化，请刷新后重试")

// refundOne 退一张单，按真实状态走对的接口，处理「读后被轮询器确认扣除 / 被回收」的竞态：
//   - HELD：Load 拿不到（刚变了）或 Refund 报 ErrGone → 重读真实状态再判；
//   - CONFIRMED / PARTIALLY_REFUNDED：RefundConfirmed（锁内判断，返回本次退的点数）；
//   - REFUNDED / EXPIRED / CANCELLED：已经不在用户账上，不动钱，只读流水认领归属。
//
// 返回本动作实际退回的点数（以流水为准）与最终真实状态。只有最终状态是退回类，调用方才把镜像改已退回。
func (s *Service) refundOne(ctx context.Context, userID int64, reqID string, actionID int64, reason string) (pts int64, final string, err error) {
	triedHeld := false
	for round := 0; round < 3; round++ {
		b, err := s.bill.LoadReservation(ctx, userID, reqID)
		if err != nil {
			return 0, "", err
		}
		if b == nil {
			return 0, "", fmt.Errorf("预留单不存在")
		}
		switch {
		case b.Status == resHeld:
			r, err := s.bill.Load(ctx, userID, reqID)
			if err != nil {
				return 0, b.Status, err
			}
			if r == nil {
				continue // 读与加载之间被确认扣除 / 回收了：重读真实状态
			}
			if err := s.bill.Refund(ctx, r, reason); err != nil {
				if errors.Is(err, billing.ErrGone) {
					continue
				}
				return 0, b.Status, err
			}
			triedHeld = true
		case b.Status == resConfirmed || b.Status == resPartiallyRefunded:
			n, err := s.bill.RefundConfirmed(ctx, userID, reqID, reason)
			if err != nil {
				if errors.Is(err, billing.ErrGone) {
					continue
				}
				return 0, b.Status, err
			}
			if n > 0 {
				return n, "REFUNDED", nil
			}
		case refundedLike(b.Status):
		default:
			return 0, b.Status, errStateChanged
		}
		// 退回类终态（或刚退完）：读流水认领
		cur, err := s.bill.LoadReservation(ctx, userID, reqID)
		if err != nil {
			return 0, "", err
		}
		final = b.Status
		if cur != nil {
			final = cur.Status
		}
		if !refundedLike(final) {
			continue
		}
		owned, found, err := s.ledgerOwned(ctx, userID, b.No, actionID)
		if err != nil {
			return 0, final, err
		}
		if triedHeld && final == "REFUNDED" && !found {
			logx.WithContext(ctx).Errorf("replicate admin: 冻结单退回成功但找不到退回流水 判据=ADMIN_REFUND_LEDGER_MISSING 单号=%s 请求号=%s action=%d", b.No, reqID, actionID)
			return 0, final, fmt.Errorf("退回成功但找不到计费流水（判据 ADMIN_REFUND_LEDGER_MISSING），请人工核对")
		}
		return owned, final, nil
	}
	return 0, "", errStateChanged
}

// Refund 按段人工退点（见包注释）。
//
// 逐单审计：每张单动钱前先落一条 PENDING 明细，结果确认后改 DONE（points = 计费流水里带本动作前缀的金额）/ FAILED。
// 同键重试（FAILED，或 RUNNING 但认领过期）接管同一个动作：遗留的非 DONE 明细先按真实状态对账（退回类终态或流水归属本动作才记完成），
// FAILED 明细重置为 PENDING 后按当前状态重来；expectedPoints 只与「尚未退成的单」的合计比较；DONE 明细原样保留。
func (s *Service) Refund(ctx context.Context, req RefundReq) (*RefundResp, error) {
	req.Reason = strings.TrimSpace(sanitizeReason(req.Reason))
	if req.Reason == "" {
		return nil, xerr.New(xerr.ParamMissing, "退点原因必填")
	}
	if len([]rune(req.Reason)) > 200 {
		return nil, xerr.Param("退点原因最多 200 字")
	}
	if len(req.SegIndexes) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "请选择要退点的段")
	}
	segs := append([]int(nil), req.SegIndexes...)
	sort.Ints(segs)
	segParts := make([]string, 0, len(segs))
	for i, k := range segs {
		if i > 0 && segs[i-1] == k {
			return nil, xerr.Param("段号重复")
		}
		segParts = append(segParts, strconv.Itoa(k))
	}
	j, err := s.store.GetJob(ctx, req.JobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, xerr.New(xerr.NotFound, "任务不存在")
	}
	for _, k := range segs {
		if k < 1 || k > j.SegTotal {
			return nil, xerr.Param(fmt.Sprintf("没有第 %d 段", k))
		}
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	active := activeRequests(steps)
	want := map[int]bool{}
	for _, k := range segs {
		want[k] = true
	}
	type target struct {
		c   *model.Charge
		row ChargeRow
	}
	var all []target
	var nos []string
	rowOf := map[string]ChargeRow{}
	for _, c := range charges {
		if !want[c.SegIndex] {
			continue
		}
		row, err := s.chargeRow(ctx, j, c, active)
		if err != nil {
			return nil, err
		}
		all = append(all, target{c, row})
		rowOf[c.RequestID] = row
		if row.ReservationNo != "" {
			nos = append(nos, row.ReservationNo)
		}
	}
	sort.Strings(nos)
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = deriveKey("auto:", "refund", strings.Join(segParts, ","), req.Reason, strings.Join(nos, ","))
	}
	if len(key) > 64 {
		return nil, xerr.Param("Idempotency-Key 最多 64 字符")
	}
	existing, err := s.store.GetAdminAction(ctx, req.JobID, model.AdminActionRefund, key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		switch {
		case existing.Status == model.AdminActionDone:
			return s.replay(ctx, existing)
		case existing.Status == model.AdminActionRunning && existing.LeaseUntil != nil && existing.LeaseUntil.After(time.Now()):
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "这次退点正在处理，请稍后刷新")
		}
	}
	if !s.bill.On() {
		return nil, xerr.NewReason(xerr.NotReady, ReasonBillingOff, "后台计费未开启（ReplicateAdmin.Billing.Mode 须为 on），不能人工退点")
	}
	var targets []target
	var total int64
	for _, t := range all {
		if t.row.Refundable > 0 {
			targets = append(targets, t)
			total += t.row.Refundable
		}
	}
	if total == 0 && existing == nil {
		return nil, xerr.NewReason(xerr.Conflict, ReasonNothingToRefund, "所选段现在没有可退的点数（已退回 / 没扣点 / 还在生成中）")
	}
	if total > 0 && req.ExpectedPoints != total {
		return nil, xerr.NewReason(xerr.Conflict, ReasonPointsChanged, fmt.Sprintf("可退点数有变化：%d → %d，请刷新后再确认", req.ExpectedPoints, total))
	}
	token := newToken()
	var act *model.AdminAction
	if existing == nil {
		got, created, err := s.store.BeginAdminAction(ctx, &model.AdminAction{JobID: j.ID, AdminID: req.AdminID, Action: model.AdminActionRefund,
			SegIndex: segs[0], RequestKey: key, Reason: req.Reason, Status: model.AdminActionRunning, LeaseToken: &token}, actionLease)
		if err != nil {
			return nil, err
		}
		if !created {
			if got.Status == model.AdminActionDone {
				return s.replay(ctx, got)
			}
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "这次退点正在处理，请稍后刷新")
		}
		act = got
	} else {
		ok, err := s.store.TakeOverAdminAction(ctx, existing.ID, token, actionLease)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "这次退点正在处理，请稍后刷新")
		}
		act = existing
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workTimeout)
	defer cancel()
	lost := false // 认领被接管：旧执行者不再写结果
	finishLine := func(id int64, status string, pts int64, msg string) {
		if ok, err := s.store.FinishRefundLine(dctx, id, token, status, pts, msg); err == nil && !ok && !lost {
			lost = true
			logx.WithContext(ctx).Infof("replicate admin: 退点动作已被接管，本执行者不再写结果 action=%d", act.ID)
		}
	}
	// 接管：遗留的非 DONE 明细对账——仍可退的随下面的目标重来；其余只有「退回类终态」或「流水归属本动作」才记完成，
	// 否则保持 FAILED 并写明原因（如段又在生成、状态未知）
	if existing != nil {
		lines, err := s.store.ListRefundLines(dctx, act.ID)
		if err != nil {
			return nil, err
		}
		retarget := map[string]bool{}
		for _, t := range targets {
			retarget[t.c.RequestID] = true
		}
		for _, l := range lines {
			if l.Status == model.RefundLineDone || retarget[l.RequestID] {
				continue
			}
			b, err := s.bill.LoadReservation(dctx, j.UserID, l.RequestID)
			if err != nil {
				return nil, err
			}
			st := ""
			if b != nil {
				st = b.Status
			}
			pts, _, err := s.ledgerOwned(dctx, j.UserID, l.ReservationNo, act.ID)
			if err != nil {
				return nil, err
			}
			switch {
			case pts > 0 || refundedLike(st):
				note := ""
				if pts == 0 {
					note = "已被其它途径退回 / 结清（不是本次操作退的）"
				}
				finishLine(l.ID, model.RefundLineDone, pts, note)
			default:
				why := rowOf[l.RequestID].BlockedReason
				if why == "" {
					why = "当前真实状态 " + st + "，现在不能退"
				}
				finishLine(l.ID, model.RefundLineFailed, 0, why)
			}
		}
	}
	reason := heldReasonPrefix(act.ID) + req.Reason
	var this int64
	for _, t := range targets {
		line, err := s.store.UpsertRefundLine(dctx, &model.AdminRefundLine{ActionID: act.ID, JobID: j.ID, SegIndex: t.c.SegIndex,
			RequestID: t.c.RequestID, ReservationNo: t.row.ReservationNo, BeforeStatus: t.row.BillingStatus,
			ExpectedPoints: t.row.Refundable, Status: model.RefundLinePending})
		if err != nil {
			logx.WithContext(ctx).Errorf("replicate admin: 退点明细落库失败，这张单本次不动钱 job=%d 请求号=%s: %v", j.ID, t.c.RequestID, err)
			continue
		}
		if line.Status == model.RefundLineDone || lost {
			continue
		}
		pts, final, perr := s.refundOne(dctx, j.UserID, t.c.RequestID, act.ID, reason)
		if perr != nil {
			finishLine(line.ID, model.RefundLineFailed, 0, perr.Error())
			logx.WithContext(ctx).Errorf("replicate admin: 人工退点失败 判据=ADMIN_REFUND_FAILED job=%d 请求号=%s admin=%d: %v", j.ID, t.c.RequestID, req.AdminID, perr)
			continue
		}
		note := ""
		if pts == 0 {
			note = "已被其它途径退回 / 结清（不是本次操作退的）"
		}
		finishLine(line.ID, model.RefundLineDone, pts, note)
		this += pts
		// 镜像只在真实状态已是退回 / 释放时改已退回：绝不能出现「镜像已退回、计费其实已扣除没退」
		if refundedLike(final) {
			if _, merr := s.store.UpdateCharge(dctx, t.c.ID, []int8{model.ChargePending, model.ChargeHeld, model.ChargeConfirmed, model.ChargeSettleFailed},
				map[string]any{"status": model.ChargeRefunded, "reason": trim("admin: "+req.Reason, 250), "settled_at": time.Now()}); merr != nil {
				logx.WithContext(ctx).Errorf("replicate admin: 退点成功但写镜像失败（下次详情会显示不一致） job=%d 请求号=%s: %v", j.ID, t.c.RequestID, merr)
			}
		}
	}
	lines, err := s.store.ListRefundLines(dctx, act.ID)
	if err != nil {
		return nil, err
	}
	resp := &RefundResp{ActionID: act.ID, ThisPoints: this, Lines: lines, Status: model.AdminActionDone}
	failed := 0
	for _, l := range lines {
		if l.Status == model.RefundLineDone {
			resp.Points += l.Points
		} else {
			failed++
		}
	}
	if failed > 0 {
		resp.Status = model.AdminActionFailed
	}
	if !lost {
		b, _ := json.Marshal(lines)
		if ok, err := s.store.FinishAdminAction(dctx, act.ID, token, resp.Status, resp.Points, b); err != nil || !ok {
			logx.WithContext(ctx).Infof("replicate admin: 写审计结果没成功（已被接管或库错误） action=%d ok=%v err=%v", act.ID, ok, err)
		}
	}
	logx.WithContext(ctx).Infof("replicate admin: 人工退点 job=%d admin=%d 段=%v 本次退回=%d 累计=%d 失败=%d 原因=%s", j.ID, req.AdminID, segs, this, resp.Points, failed, req.Reason)
	if failed > 0 {
		return resp, &PartialError{Resp: resp, Msg: fmt.Sprintf("有 %d 张预留单没退成（这次已退回 %d 点，累计 %d 点）。刷新页面后用同一请求重试，只会补退剩下的", failed, this, resp.Points)}
	}
	return resp, nil
}

func newToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- 强制收尾

// ForceReq POST /jobs/{id}/force-finish。
type ForceReq struct {
	JobID          int64
	AdminID        int64
	Reason         string
	IdempotencyKey string // 只认请求头 Idempotency-Key；空 = 按 原因 + job 状态 + 当前未结镜像集合 派生
}

// ForceResp 强制收尾结果。
type ForceResp struct {
	ActionID int64 `json:"actionId"`
	Replay   bool  `json:"replay"`
	*core.ForceFinishResult
}

// ForceFinish 强制收尾（只收非在途；未结镜像先按步骤状态结清，见 core.ForceFinish）。
func (s *Service) ForceFinish(ctx context.Context, req ForceReq) (*ForceResp, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		return nil, xerr.New(xerr.ParamMissing, "收尾原因必填")
	}
	if len([]rune(req.Reason)) > 200 {
		return nil, xerr.Param("收尾原因最多 200 字")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		j, err := s.store.GetJob(ctx, req.JobID)
		if err != nil {
			return nil, err
		}
		if j == nil {
			return nil, xerr.New(xerr.NotFound, "任务不存在")
		}
		charges, err := s.store.ListCharges(ctx, req.JobID)
		if err != nil {
			return nil, err
		}
		var open []string
		for _, c := range charges {
			if model.ChargeOpen(c.Status) {
				open = append(open, strconv.FormatInt(c.ID, 10))
			}
		}
		sort.Strings(open)
		key = deriveKey("auto:", "force", req.Reason, strconv.Itoa(int(j.Status)), strings.Join(open, ","))
	}
	if len(key) > 64 {
		return nil, xerr.Param("Idempotency-Key 最多 64 字符")
	}
	existing, err := s.store.GetAdminAction(ctx, req.JobID, model.AdminActionForceFinish, key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		switch {
		case existing.Status == model.AdminActionDone:
			out := &ForceResp{ActionID: existing.ID, Replay: true, ForceFinishResult: &core.ForceFinishResult{}}
			_ = json.Unmarshal(existing.Result, out.ForceFinishResult)
			return out, nil
		case existing.Status == model.AdminActionRunning && existing.LeaseUntil != nil && existing.LeaseUntil.After(time.Now()):
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "收尾正在处理，请稍后刷新")
		}
	}
	if !s.bill.On() {
		return nil, xerr.NewReason(xerr.NotReady, ReasonBillingOff, "后台计费未开启（ReplicateAdmin.Billing.Mode 须为 on），不能收尾动钱")
	}
	if s.forcer == nil {
		return nil, xerr.New(xerr.NotReady, "功能暂未开放")
	}
	token := newToken()
	var act *model.AdminAction
	if existing == nil {
		got, created, err := s.store.BeginAdminAction(ctx, &model.AdminAction{JobID: req.JobID, AdminID: req.AdminID, Action: model.AdminActionForceFinish,
			RequestKey: key, Reason: req.Reason, Status: model.AdminActionRunning, LeaseToken: &token}, actionLease)
		if err != nil {
			return nil, err
		}
		if !created {
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "收尾正在处理，请稍后刷新")
		}
		act = got
	} else {
		ok, err := s.store.TakeOverAdminAction(ctx, existing.ID, token, actionLease)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, xerr.NewReason(xerr.Conflict, ReasonInProgress, "收尾正在处理，请稍后刷新")
		}
		act = existing
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workTimeout)
	defer cancel()
	res, ferr := s.forcer.ForceFinish(dctx, req.JobID)
	status := model.AdminActionDone
	var body []byte
	if ferr != nil {
		status = model.AdminActionFailed
		body, _ = json.Marshal(map[string]string{"error": ferr.Error()})
	} else {
		body, _ = json.Marshal(res)
	}
	if ok, err := s.store.FinishAdminAction(dctx, act.ID, token, status, 0, body); err != nil || !ok {
		logx.WithContext(ctx).Infof("replicate admin: 写审计结果没成功（已被接管或库错误） action=%d ok=%v err=%v", act.ID, ok, err)
	}
	logx.WithContext(ctx).Infof("replicate admin: 强制收尾 job=%d admin=%d 原因=%s err=%v", req.JobID, req.AdminID, req.Reason, ferr)
	if ferr != nil {
		return nil, ferr
	}
	return &ForceResp{ActionID: act.ID, ForceFinishResult: res}, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func trim(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
