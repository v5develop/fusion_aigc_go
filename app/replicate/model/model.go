// Package model 视频做同款（afv_replicate_*，deploy/sql/Go_V1.0.37__replicate.sql）的 bun 结构体与状态常量。
//
// 只放结构体与常量；数据访问（插入、CAS 迁移）在 app/replicate/repo。
// 写 Java 共享表（afv_billing_*、afv_creation_task、afv_asset、afv_task_event）一律走对应 pkg，不在这里建模。
package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 表名。
const (
	TableAnalysis = "afv_replicate_analysis"
	TablePlan     = "afv_replicate_plan"
	TableJob      = "afv_replicate_job"
	TableStep     = "afv_replicate_step"
	TableCharge   = "afv_replicate_charge"
	TablePref     = "afv_replicate_user_pref"
)

// 分析状态。
const (
	AnalysisQueued    int8 = 0
	AnalysisRunning   int8 = 1
	AnalysisSucceeded int8 = 2
	AnalysisFailed    int8 = 3
)

// 分析阶段（顺序即推进顺序）。
const (
	StageIngest     = "ingest"
	StageProbe      = "probe"
	StageFrames     = "frames"
	StageSheet      = "sheet"
	StageTranscribe = "transcribe"
	StageVision     = "vision"
	StageDone       = "done"
)

// AnalysisStages 阶段推进顺序。
var AnalysisStages = []string{StageIngest, StageProbe, StageFrames, StageSheet, StageTranscribe, StageVision, StageDone}

// Analysis afv_replicate_analysis。
type Analysis struct {
	bun.BaseModel `bun:"table:afv_replicate_analysis,alias:a"`

	ID         int64           `bun:"id,pk,autoincrement"`
	AnalysisNo string          `bun:"analysis_no"`
	UserID     int64           `bun:"user_id"`
	SourceKey  string          `bun:"source_key"`
	SourceURL  string          `bun:"source_url"`
	SourceRid  *string         `bun:"source_rid"`
	Status     int8            `bun:"status"`
	Stage      string          `bun:"stage"`
	Attempt    int             `bun:"attempt"`
	State      json.RawMessage `bun:"state,type:json,nullzero"`
	Result     json.RawMessage `bun:"result,type:json,nullzero"`
	ErrorCode  *string         `bun:"error_code"`
	ErrorMsg   *string         `bun:"error_msg"`
	LeaseOwner *string         `bun:"lease_owner"`
	LeaseUntil *time.Time      `bun:"lease_until"`
	// NextRunAt 轮询器下次推进时间（库时钟；NULL = 立即）。每轮推进后按退避写回，不触发 updated_at。
	NextRunAt  *time.Time `bun:"next_run_at"`
	CreatedAt  time.Time  `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt  time.Time  `bun:"updated_at,nullzero,default:current_timestamp"`
	FinishedAt *time.Time `bun:"finished_at"`
}

// 方案状态（库里只有这两个值）。过期不落库：按 expires_at 判断，视图里派生成 EXPIRED（core.PlanViewExpired）；
// 指纹不一致是确认接口的 409 reason PLAN_STALE，不是方案状态。
const (
	PlanReady     = "READY"
	PlanConfirmed = "CONFIRMED"
	// PlanStale 被改标注取代（superseded_by 指向新方案，Go_V1.0.41）：不可再确认，确认回 409 PLAN_STALE。
	PlanStale = "STALE"
)

// Plan afv_replicate_plan。
type Plan struct {
	bun.BaseModel `bun:"table:afv_replicate_plan,alias:p"`

	ID          int64           `bun:"id,pk,autoincrement"`
	PlanNo      string          `bun:"plan_no"`
	UserID      int64           `bun:"user_id"`
	AnalysisID  int64           `bun:"analysis_id"`
	SampleID    *int64          `bun:"sample_id"`      // 用示例出的方案（Go_V1.0.41）
	ParentID    *int64          `bun:"parent_plan_id"` // 改标注：由哪张方案改来
	Status      string          `bun:"status"`
	ModelGroup  string          `bun:"model_group"`
	Resolution  string          `bun:"resolution"`
	AspectRatio string          `bun:"aspect_ratio"`
	Annotations json.RawMessage `bun:"annotations,type:json"`
	Refs        json.RawMessage `bun:"refs,type:json,nullzero"`
	Segments    json.RawMessage `bun:"segments,type:json"`
	Prompts     json.RawMessage `bun:"prompts,type:json"`
	Quote       json.RawMessage `bun:"quote,type:json,nullzero"`
	PlanHash    string          `bun:"plan_hash"`
	JobID       *int64          `bun:"job_id"`
	// SupersededBy 改标注：被哪张新方案取代（status=STALE）
	SupersededBy *int64    `bun:"superseded_by"`
	ExpiresAt    time.Time `bun:"expires_at"`
	CreatedAt    time.Time `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt    time.Time `bun:"updated_at,nullzero,default:current_timestamp"`
}

// 出片任务状态（0–5 与任务中心状态集一致；6/7/8 对任务中心映射为处理中）。
const (
	JobQueued     int8 = 0
	JobRunning    int8 = 1
	JobSucceeded  int8 = 2
	JobFailed     int8 = 3
	JobCanceled   int8 = 4
	JobPartial    int8 = 5
	JobSubmitting int8 = 6 // 提交中：job+步骤已落库，正在逐段预留；全部预留成功才 → 排队
	JobWaiting    int8 = 7 // 待继续：第 1 段已成功，gate/continue 等用户
	JobContinuing int8 = 8 // 继续中：正在逐段预留其余段；成功 → 执行中，失败 → 回到待继续
)

// JobFinal 终态（可被用户单段重做从 3/5 拉回执行中）。
func JobFinal(s int8) bool {
	return s == JobSucceeded || s == JobFailed || s == JobCanceled || s == JobPartial
}

// Job afv_replicate_job。
type Job struct {
	bun.BaseModel `bun:"table:afv_replicate_job,alias:j"`

	ID                int64           `bun:"id,pk,autoincrement"`
	UserID            int64           `bun:"user_id"`
	WorkspaceID       int64           `bun:"workspace_id"`
	RequestID         string          `bun:"request_id"`
	PlanID            int64           `bun:"plan_id"`
	PlanNo            string          `bun:"plan_no"`
	PlanHash          string          `bun:"plan_hash"`
	TaskID            *string         `bun:"task_id"`
	Status            int8            `bun:"status"`
	Staged            bool            `bun:"staged"`
	Input             json.RawMessage `bun:"input,type:json"`
	SegTotal          int             `bun:"seg_total"`
	SegDone           int             `bun:"seg_done"`
	SegFailed         int             `bun:"seg_failed"`
	RedoCount         int             `bun:"redo_count"`
	ContinueAttempt   int             `bun:"continue_attempt"`
	ContinueRequestID *string         `bun:"continue_request_id"`
	ContinueDeadline  *time.Time      `bun:"continue_deadline"`
	StopReason        *string         `bun:"stop_reason"`
	ResultDocument    json.RawMessage `bun:"result_document,type:json,nullzero"`
	ErrorReason       *string         `bun:"error_reason"`
	LeaseOwner        *string         `bun:"lease_owner"`
	LeaseUntil        *time.Time      `bun:"lease_until"`
	NextRunAt         *time.Time      `bun:"next_run_at"` // 轮询器下次推进时间（NULL = 立即）
	CreatedAt         time.Time       `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt         time.Time       `bun:"updated_at,nullzero,default:current_timestamp"`
	FinishedAt        *time.Time      `bun:"finished_at"`
}

// 步骤种类与状态。
const (
	StepKindGateway = "gateway"
	StepKindGate    = "gate"
	// StepKindWriter 写手步骤（seg/NN/write）：为该段写出片提示词，不挂预留单、不计入段状态（segChain 只看 gateway）。
	StepKindWriter = "writer"

	StepHolding    = "HOLDING" // 用户重做：步骤已建、预留进行中，引擎不取
	StepPending    = "PENDING"
	StepRunning    = "RUNNING"
	StepDelivering = "DELIVERING" // 已出片，正在确认扣费；只有确认成功才 → SUCCEEDED
	StepSucceeded  = "SUCCEEDED"
	StepFailed     = "FAILED"
	StepCanceled   = "CANCELLED"
	StepWaiting    = "WAITING" // gate 等用户
)

// GateKey 分阶段确认的门步骤键。
const GateKey = "gate/continue"

// StepFinal 步骤是否终态。
func StepFinal(s string) bool { return s == StepSucceeded || s == StepFailed || s == StepCanceled }

// Step afv_replicate_step。
type Step struct {
	bun.BaseModel `bun:"table:afv_replicate_step,alias:s"`

	ID               int64           `bun:"id,pk,autoincrement"`
	JobID            int64           `bun:"job_id"`
	StepKey          string          `bun:"step_key"`
	Kind             string          `bun:"kind"`
	SegIndex         int             `bun:"seg_index"`
	Status           string          `bun:"status"`
	IdemKey          string          `bun:"idem_key"`
	BillingRequestID *string         `bun:"billing_request_id"`
	Attempt          int             `bun:"attempt"`
	GatewayCallID    *string         `bun:"gateway_call_id"`
	GatewayStatus    *string         `bun:"gateway_status"`
	Output           json.RawMessage `bun:"output,type:json,nullzero"`
	ErrorClass       *string         `bun:"error_class"`
	ErrorCode        *string         `bun:"error_code"`
	ErrorMsg         *string         `bun:"error_msg"`
	RetryOf          *string         `bun:"retry_of"`
	StartedAt        *time.Time      `bun:"started_at"`
	FinishedAt       *time.Time      `bun:"finished_at"`
	LeaseOwner       *string         `bun:"lease_owner"`
	CreatedAt        time.Time       `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt        time.Time       `bun:"updated_at,nullzero,default:current_timestamp"`
}

// 预留镜像状态（afv_replicate_charge.status）。
const (
	ChargePending      int8 = 0 // 待预留（行先落、再调 billing.Hold，崩溃后回收器据此退回）
	ChargeHeld         int8 = 1
	ChargeConfirmed    int8 = 2
	ChargeRefunded     int8 = 3
	ChargeFree         int8 = 4 // 不计费：Billing.Mode=off/shadow 或 0 点
	ChargeHoldFailed   int8 = 5 // 预留失败（未扣任何点数）
	ChargeSettleFailed int8 = 6 // 扣除 / 退回调用失败，对账重做（reason 前缀区分：confirm: 扣费待重试 / refund: 退点待重试）
)

// 结算待重试的 reason 前缀（afv_replicate_charge.reason），用来区分「扣费失败」与「退点失败」。
const (
	SettleReasonConfirm = "confirm: "
	SettleReasonRefund  = "refund: "
)

// ChargeOpenStatuses 还可能动账的镜像状态（DueJobs 补扫用，与 ChargeOpen 同口径）。
var ChargeOpenStatuses = []int8{ChargePending, ChargeHeld, ChargeSettleFailed}

// ChargeOpen 还可能动账的镜像状态（需要结算或对账）。
func ChargeOpen(s int8) bool { return s == ChargePending || s == ChargeHeld || s == ChargeSettleFailed }

// 预留阶段。
const (
	ChargeStageFirst = "first"
	ChargeStageAll   = "all"
	ChargeStageRest  = "rest"
	ChargeStageRedo  = "redo"
)

// Charge afv_replicate_charge。
type Charge struct {
	bun.BaseModel `bun:"table:afv_replicate_charge,alias:c"`

	ID        int64   `bun:"id,pk,autoincrement"`
	JobID     int64   `bun:"job_id"`
	UserID    int64   `bun:"user_id"`
	SegIndex  int     `bun:"seg_index"`
	Stage     string  `bun:"stage"`
	RequestID string  `bun:"request_id"`
	ClientKey *string `bun:"client_key"`
	Seconds   int     `bun:"seconds"`
	Points    int64   `bun:"points"`
	Status    int8    `bun:"status"`
	Reason    *string `bun:"reason"`
	// HeldAt 冻结成功、镜像写成已预留的时刻（应用时钟，≥ billing 冻结时刻；写一次后不再改）。续期从它起算：
	// 镜像的 updated_at 之后会被改写（如扣费失败记 6-结算待重试），推不出冻结时刻。NULL = 旧行 / 从没冻结成功。
	HeldAt    *time.Time `bun:"held_at"`
	CreatedAt time.Time  `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt time.Time  `bun:"updated_at,nullzero,default:current_timestamp"`
	SettledAt *time.Time `bun:"settled_at"`
}

// UserPref afv_replicate_user_pref。
type UserPref struct {
	bun.BaseModel `bun:"table:afv_replicate_user_pref,alias:up"`

	UserID       int64     `bun:"user_id,pk"`
	AutoContinue bool      `bun:"auto_continue"`
	CreatedAt    time.Time `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt    time.Time `bun:"updated_at,nullzero,default:current_timestamp"`
}
