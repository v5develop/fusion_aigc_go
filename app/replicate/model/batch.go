package model

import (
	"time"

	"github.com/uptrace/bun"
)

// 批量做多条（deploy/sql/Go_V1.0.54__replicate_batch.sql）。
const (
	TableBatch    = "afv_replicate_batch"
	TableBatchJob = "afv_replicate_batch_job"
)

// 批次头状态（afv_replicate_batch.status）。
const (
	BatchSubmitting int8 = 0 // 逐条预留进行中
	BatchSubmitted  int8 = 1 // 全部预留成功、任务已排队
	BatchFailed     int8 = 2 // 预留失败，已整批回滚
)

// Batch afv_replicate_batch：一次批量确认。
type Batch struct {
	bun.BaseModel `bun:"table:afv_replicate_batch,alias:b"`

	ID           int64     `bun:"id,pk,autoincrement"`
	UserID       int64     `bun:"user_id"`
	PlanID       int64     `bun:"plan_id"`
	PlanNo       string    `bun:"plan_no"`
	RequestID    string    `bun:"request_id"`
	VariantCount int       `bun:"variant_count"`
	TotalPoints  int64     `bun:"total_points"`
	Status       int8      `bun:"status"`
	FailReason   *string   `bun:"fail_reason"`
	CreatedAt    time.Time `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt    time.Time `bun:"updated_at,nullzero,default:current_timestamp"`
}

// BatchJob afv_replicate_batch_job：批次 → 任务。
type BatchJob struct {
	bun.BaseModel `bun:"table:afv_replicate_batch_job,alias:bj"`

	ID          int64     `bun:"id,pk,autoincrement"`
	BatchID     int64     `bun:"batch_id"`
	Seq         int       `bun:"seq"`
	ChildPlanID int64     `bun:"child_plan_id"`
	JobID       int64     `bun:"job_id"`
	CreatedAt   time.Time `bun:"created_at,nullzero,default:current_timestamp"`
}
