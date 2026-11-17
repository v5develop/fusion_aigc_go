package model

import (
	"time"

	"github.com/uptrace/bun"
)

// 成片分享进示例库的用户投稿（Go_V1.0.58，CONTRACT §T）。

// TableShare 投稿表。
const TableShare = "afv_replicate_share"

// 投稿状态。
const (
	SharePending   = "pending"   // 待审核
	ShareApproved  = "approved"  // 已通过（已生成示例）
	ShareRejected  = "rejected"  // 已驳回（不可再投）
	ShareWithdrawn = "withdrawn" // 用户已撤回（可重新投）
)

// 奖励状态（积分在 Java，这里只记运营是否已发放）。
const (
	RewardNone    = "none"
	RewardGranted = "granted"
)

// Share afv_replicate_share：一条投稿。
type Share struct {
	bun.BaseModel `bun:"table:afv_replicate_share,alias:sh"`

	ID              int64      `bun:"id,pk,autoincrement"`
	UserID          int64      `bun:"user_id"`
	JobID           int64      `bun:"job_id"`
	PlanID          int64      `bun:"plan_id"`
	Status          string     `bun:"status"`
	RejectReason    string     `bun:"reject_reason"`
	SampleID        *int64     `bun:"sample_id"`
	PortraitConsent bool       `bun:"portrait_consent"`
	Industry        string     `bun:"industry"`
	RewardStatus    string     `bun:"reward_status"`
	RewardAt        *time.Time `bun:"reward_at"`
	ReviewedBy      int64      `bun:"reviewed_by"`
	ReviewedAt      *time.Time `bun:"reviewed_at"`
	CreatedAt       time.Time  `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt       time.Time  `bun:"updated_at,nullzero,default:current_timestamp"`
}
