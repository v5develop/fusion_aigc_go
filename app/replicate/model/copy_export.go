package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 发布文案 / 多比例导出（Go_V1.0.55，CONTRACT §N）。

const (
	TableCopy      = "afv_replicate_copy"
	TableCopyQuota = "afv_replicate_copy_quota"
	TableExport    = "afv_replicate_export"
)

// 文案 / 导出占坑状态。
const (
	ClaimGenerating int8 = 0 // 已占坑，正在生成（崩溃后超过回收时限可被接手）
	ClaimDone       int8 = 1 // 已完成，结果有值；后续同键调用回放
)

// Copy afv_replicate_copy：一条成片在一个平台的发布文案。
type Copy struct {
	bun.BaseModel `bun:"table:afv_replicate_copy,alias:pc"`

	ID        int64           `bun:"id,pk,autoincrement"`
	UserID    int64           `bun:"user_id"`
	JobID     int64           `bun:"job_id"`
	Platform  string          `bun:"platform"`
	Status    int8            `bun:"status"`
	Content   json.RawMessage `bun:"content,type:json,nullzero"`
	CallID    string          `bun:"call_id"`
	CreatedAt time.Time       `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt time.Time       `bun:"updated_at,nullzero,default:current_timestamp"`
}

// Export afv_replicate_export：一条成片的一种比例导出。
type Export struct {
	bun.BaseModel `bun:"table:afv_replicate_export,alias:pe"`

	ID        int64     `bun:"id,pk,autoincrement"`
	UserID    int64     `bun:"user_id"`
	JobID     int64     `bun:"job_id"`
	Ratio     string    `bun:"ratio"`
	Mode      string    `bun:"mode"`
	SrcRid    string    `bun:"src_rid"`
	Status    int8      `bun:"status"`
	VideoRid  string    `bun:"video_rid"`
	Version   int       `bun:"version"`
	CreatedAt time.Time `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,default:current_timestamp"`
}
