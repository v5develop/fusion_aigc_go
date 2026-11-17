package model

import (
	"time"

	"github.com/uptrace/bun"
)

// 换声音（CONTRACT §P）：每人每天额度 + 按任务的占用流水（Go_V1.0.57）。
const (
	TableVoiceQuota = "afv_replicate_voice_quota"
	TableVoiceUse   = "afv_replicate_voice_use"
)

// 占用流水状态。
const (
	VoiceUseHeld     int8 = 1 // 确认时占用了一次额度
	VoiceUseRefunded int8 = 2 // 已退回（任务没成功 / 合成失败）；只退一次
)

// VoiceUse afv_replicate_voice_use：一个任务占用的那一次换声音额度（job_id 唯一 = 一个任务最多占一次、最多退一次）。
type VoiceUse struct {
	bun.BaseModel `bun:"table:afv_replicate_voice_use,alias:pvu"`

	JobID     int64     `bun:"job_id,pk"`
	UserID    int64     `bun:"user_id"`
	Day       string    `bun:"day"`
	Status    int8      `bun:"status"`
	CreatedAt time.Time `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,default:current_timestamp"`
}
