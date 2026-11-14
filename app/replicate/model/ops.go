package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 上线补齐的自有表（deploy/sql/Go_V1.0.39__replicate_ops_notification.sql）。
const (
	TableModeration  = "afv_replicate_moderation"
	TableDelivery    = "afv_replicate_delivery"
	TableAdminAction = "afv_replicate_admin_action"
	TableRefundLine  = "afv_replicate_admin_refund_line"
)

// 审核结论（afv_replicate_moderation.status）。
const (
	ModerationPassed    = "PASSED"
	ModerationRejected  = "REJECTED"
	ModerationErrorOpen = "ERROR_OPEN" // 审核服务故障，按 fail-open 放行
)

// Moderation afv_replicate_moderation：一版方案（plan_id + plan_hash）的审核结论。
type Moderation struct {
	bun.BaseModel `bun:"table:afv_replicate_moderation,alias:m"`

	ID        int64           `bun:"id,pk,autoincrement" json:"id"`
	UserID    int64           `bun:"user_id" json:"userId"`
	PlanID    int64           `bun:"plan_id" json:"planId"`
	PlanNo    string          `bun:"plan_no" json:"planNo"`
	PlanHash  string          `bun:"plan_hash" json:"planHash"`
	PolicySig string          `bun:"policy_sig" json:"policySig"`
	Attempt   int             `bun:"attempt" json:"attempt"`
	Status    string          `bun:"status" json:"status"`
	Reason    string          `bun:"reason" json:"reason"`
	Detail    json.RawMessage `bun:"detail,type:json,nullzero" json:"detail"`
	CreatedAt time.Time       `bun:"created_at,nullzero,default:current_timestamp" json:"createdAt"`
	UpdatedAt time.Time       `bun:"updated_at,nullzero,default:current_timestamp" json:"updatedAt"`
}

// 入库对象（afv_replicate_delivery.kind）。
const (
	DeliveryFinal   = "final"   // 整条成片（完整版，或用户不继续 / 到期的部分成片）
	DeliveryPreview = "preview" // 预览版成片（失败段占位；同 job 只入一次，之后完整版另入一条 final，预览版保留）
	DeliverySegment = "segment" // 单段（收尾时已确认扣费的段自动入；用户手动保存走同一行）
)

// 入库状态（afv_replicate_delivery.status）。
const (
	DeliveryPending int8 = 0
	DeliveryDone    int8 = 1
	DeliveryGaveUp  int8 = 2
)

// Delivery afv_replicate_delivery：成片 / 单段入素材库发件箱。
type Delivery struct {
	bun.BaseModel `bun:"table:afv_replicate_delivery,alias:d"`

	ID         int64      `bun:"id,pk,autoincrement"`
	JobID      int64      `bun:"job_id"`
	UserID     int64      `bun:"user_id"`
	Kind       string     `bun:"kind"`
	SegIndex   int        `bun:"seg_index"`
	SrcRid     string     `bun:"src_rid"`
	CoverRid   *string    `bun:"cover_rid"`
	Status     int8       `bun:"status"`
	Attempts   int        `bun:"attempts"`
	LeaseToken *string    `bun:"lease_token"`
	LeaseUntil *time.Time `bun:"lease_until"`
	NextRunAt  *time.Time `bun:"next_run_at"`
	VideoURL   *string    `bun:"video_url"`
	CoverURL   *string    `bun:"cover_url"`
	LastError  *string    `bun:"last_error"`
	CreatedAt  time.Time  `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt  time.Time  `bun:"updated_at,nullzero,default:current_timestamp"`
	FinishedAt *time.Time `bun:"finished_at"`
}

// 后台动作（afv_replicate_admin_action.action / status）。
const (
	AdminActionRefund      = "REFUND"
	AdminActionForceFinish = "FORCE_FINISH"

	AdminActionRunning = "RUNNING"
	AdminActionDone    = "DONE"
	AdminActionFailed  = "FAILED"
)

// AdminAction afv_replicate_admin_action：后台人工动钱 / 收尾审计。
type AdminAction struct {
	bun.BaseModel `bun:"table:afv_replicate_admin_action,alias:aa"`

	ID         int64           `bun:"id,pk,autoincrement" json:"id"`
	JobID      int64           `bun:"job_id" json:"jobId"`
	AdminID    int64           `bun:"admin_id" json:"adminId"`
	Action     string          `bun:"action" json:"action"`
	SegIndex   int             `bun:"seg_index" json:"segIndex"`
	RequestKey string          `bun:"request_key" json:"requestKey"`
	Reason     string          `bun:"reason" json:"reason"`
	Points     int64           `bun:"points" json:"points"`
	Status     string          `bun:"status" json:"status"`
	LeaseUntil *time.Time      `bun:"lease_until" json:"leaseUntil,omitempty"`
	LeaseToken *string         `bun:"lease_token" json:"-"`
	Result     json.RawMessage `bun:"result,type:json,nullzero" json:"result"`
	CreatedAt  time.Time       `bun:"created_at,nullzero,default:current_timestamp" json:"createdAt"`
	UpdatedAt  time.Time       `bun:"updated_at,nullzero,default:current_timestamp" json:"updatedAt"`
}

// 退点明细状态（afv_replicate_admin_refund_line.status）。
const (
	RefundLinePending = "PENDING" // 动钱前先落，崩溃后据此对账
	RefundLineDone    = "DONE"
	RefundLineFailed  = "FAILED"
)

// AdminRefundLine afv_replicate_admin_refund_line：人工退点逐单明细（每张单动钱前先落 PENDING，结果确认后改 DONE / FAILED）。
type AdminRefundLine struct {
	bun.BaseModel `bun:"table:afv_replicate_admin_refund_line,alias:rl"`

	ID             int64     `bun:"id,pk,autoincrement" json:"id"`
	ActionID       int64     `bun:"action_id" json:"actionId"`
	JobID          int64     `bun:"job_id" json:"jobId"`
	SegIndex       int       `bun:"seg_index" json:"segIndex"`
	RequestID      string    `bun:"request_id" json:"requestId"`
	ReservationNo  string    `bun:"reservation_no" json:"reservationNo"`
	BeforeStatus   string    `bun:"before_status" json:"before"`
	ExpectedPoints int64     `bun:"expected_points" json:"expectedPoints"`
	Points         int64     `bun:"points" json:"points"`
	Status         string    `bun:"status" json:"status"`
	Error          *string   `bun:"error" json:"error,omitempty"`
	CreatedAt      time.Time `bun:"created_at,nullzero,default:current_timestamp" json:"createdAt"`
	UpdatedAt      time.Time `bun:"updated_at,nullzero,default:current_timestamp" json:"updatedAt"`
}
