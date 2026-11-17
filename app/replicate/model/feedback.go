package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 成片反馈（Go_V1.0.60，CONTRACT §Z）。
const TableFeedback = "afv_replicate_feedback"

// Feedback afv_replicate_feedback：用户对一条成片（seg_index=0）或某一段的满意 / 不满意；同一用户同一任务同一段只留一行，可覆盖更新。
type Feedback struct {
	bun.BaseModel `bun:"table:afv_replicate_feedback,alias:fb"`

	ID       int64  `bun:"id,pk,autoincrement"`
	UserID   int64  `bun:"user_id"`
	JobID    int64  `bun:"job_id"`
	SegIndex int    `bun:"seg_index"` // 0 = 整条成片；≥1 = 某一段
	Rating   string `bun:"rating"`    // up / down
	// Reasons 原因枚举数组（只有 down 才有）
	Reasons json.RawMessage `bun:"reasons,type:json,nullzero"`
	Note    string          `bun:"note"`
	// Continuity 该任务确认时的画面连贯度 fast / smooth（冗余存一份，便于不 join 直接按它对比）
	Continuity string    `bun:"continuity"`
	CreatedAt  time.Time `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt  time.Time `bun:"updated_at,nullzero,default:current_timestamp"`
}
