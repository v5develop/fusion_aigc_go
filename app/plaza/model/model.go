// Package model 广场模板域的 bun 结构体（迁移 deploy/sql/Go_V1.0.33__plaza_industry.sql）。
//
// 约定同 app/studio/model：只放结构体与常量，数据访问在 app/plaza/core。
// JSON 列用 json.RawMessage 承接（type:json）；可空 JSON 配 nullzero 写 NULL。
//
// 表归属：afv_compat_plaza_template 由 Java 创建、Go 接管（只加列；Java 的 Agent 运行时仍读它，
// 既有列 id/category/title/description/tags_json/data_json/sort_order/enabled/create_time/update_time/deleted 的含义不变）。
package model

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 模板状态（afv_compat_plaza_template.state），与回收站 deleted 正交。
const (
	StateDraft     = "DRAFT"
	StatePublished = "PUBLISHED"
	StateOffline   = "OFFLINE"
)

// 媒体类型。
const (
	MediaImage = "image"
	MediaVideo = "video"
)

// 视频转码 / 抽帧状态（media_status）。
const (
	// MediaStatusPending 旧值（封面没抽出来、无人推进）：Go_V1.0.46 起存量行已回填为 PROCESSING，新流程不再写它。
	MediaStatusPending = "PENDING"
	// MediaStatusProcessing 视频换了地址 / 刚确认上传后进入，由后台任务抽封面、转码，完成置 READY，重试耗尽置 FAILED。
	MediaStatusProcessing = "PROCESSING"
	MediaStatusReady      = "READY"
	MediaStatusFailed     = "FAILED"
)

// Industry 行业（一级 parent_id 为 NULL，最多两级）。
type Industry struct {
	bun.BaseModel `bun:"table:afv_template_industry,alias:ti"`

	ID              int64           `bun:"id,pk,autoincrement"`
	Code            string          `bun:"code,notnull"`
	Name            string          `bun:"name,notnull"`
	ParentID        sql.NullInt64   `bun:"parent_id"`
	IconURL         sql.NullString  `bun:"icon_url"`
	CoverURL        sql.NullString  `bun:"cover_url"`
	Hint            sql.NullString  `bun:"hint"`
	AudienceName    sql.NullString  `bun:"audience_name"`
	ExamplesJSON    json.RawMessage `bun:"examples_json,type:json,nullzero"`
	NextActionsJSON json.RawMessage `bun:"next_actions_json,type:json,nullzero"`
	SortOrder       int             `bun:"sort_order,notnull"`
	Enabled         int8            `bun:"enabled,notnull"`
	ShowWhenEmpty   int8            `bun:"show_when_empty,notnull"`
	CreatedBy       sql.NullInt64   `bun:"created_by"`
	UpdatedBy       sql.NullInt64   `bun:"updated_by"`
	CreateTime      time.Time       `bun:"create_time,notnull,default:current_timestamp"`
	UpdateTime      time.Time       `bun:"update_time,notnull,default:current_timestamp"`
	Deleted         int8            `bun:"deleted,notnull"`
}

// TemplateIndustry 模板与行业关系（物理删除，不做逻辑删除）。
type TemplateIndustry struct {
	bun.BaseModel `bun:"table:afv_plaza_template_industry,alias:pti"`

	ID         int64     `bun:"id,pk,autoincrement"`
	TemplateID int64     `bun:"template_id,notnull"`
	IndustryID int64     `bun:"industry_id,notnull"`
	SortOrder  int       `bun:"sort_order,notnull"`
	CreateTime time.Time `bun:"create_time,notnull,default:current_timestamp"`
}

// UserIndustryPref 用户行业偏好（v1 只写一行且 is_primary=1）。
type UserIndustryPref struct {
	bun.BaseModel `bun:"table:afv_user_industry_pref,alias:uip"`

	ID         int64     `bun:"id,pk,autoincrement"`
	UserID     int64     `bun:"user_id,notnull"`
	IndustryID int64     `bun:"industry_id,notnull"`
	IsPrimary  int8      `bun:"is_primary,notnull"`
	SortOrder  int       `bun:"sort_order,notnull"`
	CreateTime time.Time `bun:"create_time,notnull,default:current_timestamp"`
	UpdateTime time.Time `bun:"update_time,notnull,default:current_timestamp"`
}

// Template 广场模板（afv_compat_plaza_template，接管表）。
//
// 写入一律走 core 里的定向列更新 / 显式列插入：不要 updateById 整行实体写回
// （默认值列会连带覆盖，见 memory「@Builder.Default+updateById 覆写陷阱」）。
type Template struct {
	bun.BaseModel `bun:"table:afv_compat_plaza_template,alias:t"`

	ID                int64           `bun:"id,pk,autoincrement"`
	Category          sql.NullString  `bun:"category"`
	Title             string          `bun:"title,notnull"`
	Description       sql.NullString  `bun:"description"`
	TagsJSON          json.RawMessage `bun:"tags_json,type:json,nullzero"`
	DataJSON          json.RawMessage `bun:"data_json,type:json,notnull"`
	SortOrder         int             `bun:"sort_order,notnull"`
	Enabled           int8            `bun:"enabled,notnull"`
	CreateTime        time.Time       `bun:"create_time,notnull,default:current_timestamp"`
	UpdateTime        time.Time       `bun:"update_time,notnull,default:current_timestamp"`
	Deleted           int8            `bun:"deleted,notnull"`
	MediaType         string          `bun:"media_type,notnull"`
	VideoURL          sql.NullString  `bun:"video_url"`
	PosterURL         sql.NullString  `bun:"poster_url"`
	State             string          `bun:"state,notnull"`
	OnlineAt          sql.NullTime    `bun:"online_at"`
	OfflineAt         sql.NullTime    `bun:"offline_at"`
	ChannelsJSON      json.RawMessage `bun:"channels_json,type:json,nullzero"`
	SlotsJSON         json.RawMessage `bun:"slots_json,type:json,nullzero"`
	NeedsProductImage int8            `bun:"needs_product_image,notnull"`
	TextOverlay       int8            `bun:"text_overlay,notnull"`
	MediaStatus       string          `bun:"media_status,notnull"`
	// 视频处理流水线（Go_V1.0.46）：失败原因 / 结果备注 / 已尝试次数 / 下次重试 / 行租约。
	MediaError      sql.NullString `bun:"media_error"`
	MediaNote       sql.NullString `bun:"media_note"`
	MediaAttempts   int            `bun:"media_attempts,notnull"`
	MediaNextAt     sql.NullTime   `bun:"media_next_at"`
	MediaLeaseUntil sql.NullTime   `bun:"media_lease_until"`
	MediaLeaseToken sql.NullString `bun:"media_lease_token"`
	CreatedBy       sql.NullInt64  `bun:"created_by"`
	UpdatedBy       sql.NullInt64  `bun:"updated_by"`
	DeletedAt       sql.NullTime   `bun:"deleted_at"`
	DeletedBy       sql.NullInt64  `bun:"deleted_by"`
	// Revision 乐观并发版本号（Go_V1.0.33 加列）：Go 每次写 +1，编辑接口按它判冲突。
	Revision int `bun:"revision,notnull"`
}

// AuditLog 广场域审计（Go 独写）。
type AuditLog struct {
	bun.BaseModel `bun:"table:afv_plaza_audit_log,alias:al"`

	ID           int64           `bun:"id,pk,autoincrement"`
	OperatorID   sql.NullInt64   `bun:"operator_id"`
	OperatorName sql.NullString  `bun:"operator_name"`
	Action       string          `bun:"action,notnull"`
	ResourceType string          `bun:"resource_type,notnull"`
	ResourceID   sql.NullString  `bun:"resource_id"`
	BatchID      sql.NullString  `bun:"batch_id"`
	BeforeJSON   json.RawMessage `bun:"before_json,type:json,nullzero"`
	AfterJSON    json.RawMessage `bun:"after_json,type:json,nullzero"`
	DiffJSON     json.RawMessage `bun:"diff_json,type:json,nullzero"`
	Status       string          `bun:"status,notnull"`
	ErrorMessage sql.NullString  `bun:"error_message"`
	RequestID    sql.NullString  `bun:"request_id"`
	ClientIP     sql.NullString  `bun:"client_ip"`
	CreateTime   time.Time       `bun:"create_time,notnull,default:current_timestamp"`
}

// Channel 投放渠道字典。
type Channel struct {
	bun.BaseModel `bun:"table:afv_plaza_channel,alias:ch"`

	ID         int64          `bun:"id,pk,autoincrement"`
	Code       string         `bun:"code,notnull"`
	Name       string         `bun:"name,notnull"`
	MediaType  string         `bun:"media_type,notnull"`
	Width      int            `bun:"width,notnull"`
	Height     int            `bun:"height,notnull"`
	Ratio      string         `bun:"ratio,notnull"`
	Note       sql.NullString `bun:"note"`
	SortOrder  int            `bun:"sort_order,notnull"`
	Enabled    int8           `bun:"enabled,notnull"`
	CreateTime time.Time      `bun:"create_time,notnull,default:current_timestamp"`
	UpdateTime time.Time      `bun:"update_time,notnull,default:current_timestamp"`
	// Deleted 逻辑删除（Go_V1.0.46）；code 唯一键不变，已删除的 code 不可复用。
	Deleted   int8         `bun:"deleted,notnull"`
	DeletedAt sql.NullTime `bun:"deleted_at"`
}

// IndustrySuggestion 行业建议（匿名）。
type IndustrySuggestion struct {
	bun.BaseModel `bun:"table:afv_plaza_industry_suggestion,alias:s"`

	ID         int64          `bun:"id,pk,autoincrement"`
	Text       sql.NullString `bun:"text"`
	NormText   sql.NullString `bun:"norm_text"`
	Sid        string         `bun:"sid,notnull"`
	UserID     sql.NullInt64  `bun:"user_id"`
	IPHash     string         `bun:"ip_hash,notnull"`
	CreateTime time.Time      `bun:"create_time,notnull,default:current_timestamp"`
}

// SuggestionStatus 行业建议处理状态（按规范化文本）。
type SuggestionStatus struct {
	bun.BaseModel `bun:"table:afv_plaza_industry_suggestion_status,alias:ss"`

	NormText   string        `bun:"norm_text,pk"`
	Status     string        `bun:"status,notnull"`
	IndustryID sql.NullInt64 `bun:"industry_id"`
	HandledBy  sql.NullInt64 `bun:"handled_by"`
	HandledAt  sql.NullTime  `bun:"handled_at"`
}

// SearchMissTerm 零结果搜索词汇总。
type SearchMissTerm struct {
	bun.BaseModel `bun:"table:afv_plaza_search_miss_term,alias:mt"`

	IndustryCode string        `bun:"industry_code,pk"`
	NormTerm     string        `bun:"norm_term,pk"`
	TotalCnt     int           `bun:"total_cnt,notnull"`
	FirstDay     time.Time     `bun:"first_day,notnull"`
	LastDay      time.Time     `bun:"last_day,notnull"`
	Status       string        `bun:"status,notnull"`
	HandledBy    sql.NullInt64 `bun:"handled_by"`
	HandledAt    sql.NullTime  `bun:"handled_at"`
}

// 示例素材范围。
const (
	SampleScopeIndustry = "INDUSTRY"
	SampleScopeTemplate = "TEMPLATE"

	// 示例素材角色：放进去的素材 / 做出来的效果。
	SampleRoleInput  = "INPUT"
	SampleRoleOutput = "OUTPUT"
)

// SampleAsset 示例素材（行业级或模板级）。
type SampleAsset struct {
	bun.BaseModel `bun:"table:afv_plaza_sample_asset,alias:sa"`

	ID         int64          `bun:"id,pk,autoincrement"`
	ScopeType  string         `bun:"scope_type,notnull"`
	IndustryID sql.NullInt64  `bun:"industry_id"`
	TemplateID sql.NullInt64  `bun:"template_id"`
	Kind       string         `bun:"kind,notnull"`
	URL        string         `bun:"url,notnull"`
	ThumbURL   sql.NullString `bun:"thumb_url"`
	Title      string         `bun:"title,notnull"`
	Role       string         `bun:"role,notnull"` // INPUT | OUTPUT（缺省 OUTPUT）
	SortOrder  int            `bun:"sort_order,notnull"`
	Enabled    int8           `bun:"enabled,notnull"`
	CreatedBy  sql.NullInt64  `bun:"created_by"`
	CreateTime time.Time      `bun:"create_time,notnull,default:current_timestamp"`
	UpdateTime time.Time      `bun:"update_time,notnull,default:current_timestamp"`
	Deleted    int8           `bun:"deleted,notnull"`
}
