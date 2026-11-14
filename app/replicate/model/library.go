package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 示例视频与我的方案（deploy/sql/Go_V1.0.41__replicate_samples.sql）。
const (
	TableSample    = "afv_replicate_sample"
	TableSavedPlan = "afv_replicate_saved_plan"
)

// SystemUserID 示例分析的系统身份（afv_replicate_analysis.user_id=0）：不属于任何用户，用户只能经 sampleId 引用。
const SystemUserID int64 = 0

// MaxSavedPlans 每人「我的方案」上限（EXPERIENCE §3.9.2）。
const MaxSavedPlans = 50

// Sample afv_replicate_sample：运营登记的示例视频。
type Sample struct {
	bun.BaseModel `bun:"table:afv_replicate_sample,alias:sm"`

	ID                int64           `bun:"id,pk,autoincrement"`
	Title             string          `bun:"title"`
	Industry          string          `bun:"industry"`
	SourceRid         string          `bun:"source_rid"`
	CoverRid          *string         `bun:"cover_rid"`
	ResultRid         *string         `bun:"result_rid"`
	Annotations       json.RawMessage `bun:"annotations,type:json,nullzero"`
	SortOrder         int             `bun:"sort_order"`
	Enabled           bool            `bun:"enabled"`
	AnalysisID        *int64          `bun:"analysis_id"`
	PendingAnalysisID *int64          `bun:"pending_analysis_id"`
	AnalysisRound     int             `bun:"analysis_round"`
	AnalysisError     *string         `bun:"analysis_error"`
	AnalyzedAt        *time.Time      `bun:"analyzed_at"`
	CreatedBy         int64           `bun:"created_by"`
	UpdatedBy         int64           `bun:"updated_by"`
	Deleted           bool            `bun:"deleted"`
	CreatedAt         time.Time       `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt         time.Time       `bun:"updated_at,nullzero,default:current_timestamp"`
}

// SavedPlan afv_replicate_saved_plan：用户保存的方案（改法 + 原片引用）。
type SavedPlan struct {
	bun.BaseModel `bun:"table:afv_replicate_saved_plan,alias:sp"`

	ID             int64           `bun:"id,pk,autoincrement"`
	UserID         int64           `bun:"user_id"`
	Name           string          `bun:"name"`
	PlanID         int64           `bun:"plan_id"`
	PlanNo         string          `bun:"plan_no"`
	AnalysisID     int64           `bun:"analysis_id"`
	SampleID       *int64          `bun:"sample_id"`
	Annotations    json.RawMessage `bun:"annotations,type:json"`
	Refs           json.RawMessage `bun:"refs,type:json,nullzero"`
	ModelGroup     string          `bun:"model_group"`
	Resolution     string          `bun:"resolution"`
	AspectRatio    string          `bun:"aspect_ratio"`
	Title          string          `bun:"title"`
	SourceThumbRid *string         `bun:"source_thumb_rid"`
	UseCount       int             `bun:"use_count"`
	LastUsedAt     *time.Time      `bun:"last_used_at"`
	CreatedAt      time.Time       `bun:"created_at,nullzero,default:current_timestamp"`
	UpdatedAt      time.Time       `bun:"updated_at,nullzero,default:current_timestamp"`
}
