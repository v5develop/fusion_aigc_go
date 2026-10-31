// Package model 画布四张表（deploy/sql/Go_V1.0.25__canvas.sql）的 bun 结构体与常量。
//
// 与 app/studio/model 同一套约定：新模块业务数据访问用 bun（设计方案 §9.5），这里只放结构体与常量，
// 数据访问在 app/canvas/rpc/internal/repo。JSON 列用 json.RawMessage 承接；DATETIME(3) 零值插入走库默认。
// 写 Java 共享表（afv_creation_task、afv_asset、afv_billing_*）仍走 pkg/taskcenter、pkg/asset、pkg/billing。
package model

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// RunKind 节点执行类型（afv_canvas_node_run.kind）。
type RunKind string

const (
	KindImage        RunKind = "image"         // 图片生成：gateway image.generate / image.edit
	KindVideo        RunKind = "video"         // 视频生成：gateway video.generate
	KindToolCrop     RunKind = "tool_crop"     // 裁剪：media image/crop，不计费
	KindToolUpscale  RunKind = "tool_upscale"  // 高清放大：Java ai-image-upscaler
	KindToolOutpaint RunKind = "tool_outpaint" // 扩图：Java ai-ratio-convert
	KindToolInpaint  RunKind = "tool_inpaint"  // 局部重绘：Java ai-inpaint
	KindToolI2P      RunKind = "tool_i2p"      // 反推提示词：gateway chat.completion 多模态
	KindToolTryon    RunKind = "tool_tryon"    // 模特试穿：studio-rpc SubmitTryon
)

// Kinds 全部执行类型（固定顺序）。
var Kinds = []RunKind{KindImage, KindVideo, KindToolCrop, KindToolUpscale, KindToolOutpaint, KindToolInpaint, KindToolI2P, KindToolTryon}

// Valid 是否已知类型。
func (k RunKind) Valid() bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Backend 执行后端（afv_canvas_node_run.backend）。
type Backend string

const (
	BackendGateway Backend = "gateway" // ai-gateway：image / video / tool_i2p，Go 计费
	BackendMedia   Backend = "media"   // 媒体服务：tool_crop，同步完成，不计费
	BackendJava    Backend = "java"    // Java 兼容工具：upscale / outpaint / inpaint，Java 计费
	BackendStudio  Backend = "studio"  // studio 商拍链路：tool_tryon，studio 计费
)

// BackendOf 执行类型 → 后端（后端分派表的唯一出处）。
func BackendOf(k RunKind) Backend {
	switch k {
	case KindImage, KindVideo, KindToolI2P:
		return BackendGateway
	case KindToolCrop:
		return BackendMedia
	case KindToolUpscale, KindToolOutpaint, KindToolInpaint:
		return BackendJava
	case KindToolTryon:
		return BackendStudio
	}
	return ""
}

// run 状态（afv_canvas_node_run.status）。
const (
	RunQueued    int8 = 0 // 排队：已收单（gateway 后端已冻结点数），待提交后端
	RunRunning   int8 = 1 // 运行中：已提交后端，由轮询器推进
	RunSucceeded int8 = 2 // 成功：已转存、已结算、已入素材库
	RunFailed    int8 = 3 // 失败：gateway 后端已退款
	RunCanceled  int8 = 4 // 已取消：gateway 后端已退款
	// RunSubmitting 提交中：已落库（task_id / billing_request_id 已生成），正在冻结点数；冻结成功后一次 CAS 转排队。
	// 轮询器的排队 / 运行中扫描都不看它，只做兜底：超过 5 分钟仍提交中 → 退回可能已冻结的点数并置失败。
	RunSubmitting int8 = 5
)

// RunActive 未结束的状态（同一节点同一时刻只允许一个）。
var RunActive = []int8{RunSubmitting, RunQueued, RunRunning}

// RunFinal 是否已进终态。
func RunFinal(s int8) bool { return s == RunSucceeded || s == RunFailed || s == RunCanceled }

// StatusText run 状态 → 接口字面量（与前端 RunStatus 一致）。
func StatusText(s int8) string {
	switch s {
	case RunSubmitting:
		return "submitting"
	case RunQueued:
		return "queued"
	case RunRunning:
		return "running"
	case RunSucceeded:
		return "succeeded"
	case RunFailed:
		return "failed"
	case RunCanceled:
		return "cancelled"
	}
	return "queued"
}

// ParseStatus 接口字面量 → run 状态；不认识返回 -1。
func ParseStatus(s string) int8 {
	switch s {
	case "submitting":
		return RunSubmitting
	case "queued":
		return RunQueued
	case "running":
		return RunRunning
	case "succeeded":
		return RunSucceeded
	case "failed":
		return RunFailed
	case "cancelled", "canceled":
		return RunCanceled
	}
	return -1
}

// 计费镜像（afv_canvas_node_run.billing_status）。权威状态在 Java 预留单表。
const (
	BillNone    = "none"    // 不计费（裁剪 / Billing.Mode=off）
	BillHeld    = "held"    // 已冻结
	BillSettled = "settled" // 已结算
	// BillUnsettled 已交付但确认扣费失败（预留单已过期回收 / 已退 / 写库失败）：不重复交付，待人工对账。
	BillUnsettled = "unsettled"
	BillRefunded  = "refunded" // 已退回
	BillShadow    = "shadow"   // 影子模式：只算价不动账
	BillJava      = "java"     // Java 兼容工具自行计费，Go 不 Hold
	BillStudio    = "studio"   // studio 商拍链路自行计费
	BillReview    = "review"   // gateway UNKNOWN：不退款，待人工对账
)

// Canvas afv_canvas：一张画布。
type Canvas struct {
	bun.BaseModel `bun:"table:afv_canvas,alias:c"`

	ID          string          `bun:"id,pk"`
	UserID      int64           `bun:"user_id,notnull"`
	WorkspaceID int64           `bun:"workspace_id,notnull"`
	Title       string          `bun:"title,notnull"`
	Revision    string          `bun:"revision,notnull"`
	NodeCount   int32           `bun:"node_count,notnull"`
	CoverRid    sql.NullString  `bun:"cover_rid"`
	CoverURL    sql.NullString  `bun:"cover_url"`
	Snapshot    json.RawMessage `bun:"snapshot,type:json,notnull"`
	Deleted     int8            `bun:"deleted,notnull"`
	CreatedAt   time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt   time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// Candidate afv_canvas_node_run.candidates 的一项。RID 为媒体服务资源（素材库桶），URL 为长期可读地址。
type Candidate struct {
	RID string `json:"rid,omitempty"`
	URL string `json:"url"`
	W   int    `json:"w,omitempty"`
	H   int    `json:"h,omitempty"`
}

// NodeRun afv_canvas_node_run：一次节点执行。
type NodeRun struct {
	bun.BaseModel `bun:"table:afv_canvas_node_run,alias:r"`

	ID               int64           `bun:"id,pk,autoincrement"`
	CanvasID         string          `bun:"canvas_id,notnull"`
	NodeID           string          `bun:"node_id,notnull"`
	UserID           int64           `bun:"user_id,notnull"`
	RunID            string          `bun:"run_id,notnull"`
	Kind             RunKind         `bun:"kind,notnull"`
	Request          json.RawMessage `bun:"request,type:json,notnull"`
	ModelGroup       string          `bun:"model_group,notnull"`
	Backend          Backend         `bun:"backend,notnull"`
	BackendRef       sql.NullString  `bun:"backend_ref"`
	BackendRequest   json.RawMessage `bun:"backend_request,type:json,nullzero"` // Java 首次发送前落库的原始请求（见 logic/java_request.go）
	Status           int8            `bun:"status,notnull"`
	Progress         int32           `bun:"progress,notnull"`
	Candidates       json.RawMessage `bun:"candidates,type:json,nullzero"`
	SelectedIndex    int32           `bun:"selected_index,notnull"`
	ResultText       sql.NullString  `bun:"result_text"`
	VideoURL         sql.NullString  `bun:"video_url"`
	RequestedCount   int32           `bun:"requested_count,notnull"`
	Points           int64           `bun:"points,notnull"`
	CostCNY          string          `bun:"cost_cny,type:decimal(12,6),notnull,default:0"`
	BillingRequestID sql.NullString  `bun:"billing_request_id"`
	BillingStatus    string          `bun:"billing_status,notnull"`
	ErrorCode        sql.NullString  `bun:"error_code"`
	ErrorReason      sql.NullString  `bun:"error_reason"`
	TaskID           sql.NullString  `bun:"task_id"`
	Attempt          int32           `bun:"attempt,notnull"`
	Deleted          int8            `bun:"deleted,notnull"`
	CreatedAt        time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	StartedAt        sql.NullTime    `bun:"started_at"`
	FinishedAt       sql.NullTime    `bun:"finished_at"`
	UpdatedAt        time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// CandidateList 解出候选（解析失败按空）。
func (r *NodeRun) CandidateList() []Candidate {
	var out []Candidate
	if len(r.Candidates) > 0 {
		_ = json.Unmarshal(r.Candidates, &out)
	}
	return out
}

// Template afv_canvas_template：画布模板。
type Template struct {
	bun.BaseModel `bun:"table:afv_canvas_template,alias:t"`

	ID          int64           `bun:"id,pk,autoincrement"`
	Scope       string          `bun:"scope,notnull"` // official / user
	UserID      sql.NullInt64   `bun:"user_id"`
	Title       string          `bun:"title,notnull"`
	Description string          `bun:"description,notnull"`
	Snapshot    json.RawMessage `bun:"snapshot,type:json,notnull"`
	CoverURL    string          `bun:"cover_url,notnull"`
	Sort        int32           `bun:"sort,notnull"`
	Enabled     int8            `bun:"enabled,notnull"`
	Deleted     int8            `bun:"deleted,notnull"`
	CreatedAt   time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt   time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// 模板范围。
const (
	ScopeOfficial = "official"
	ScopeUser     = "user"
)

// PromptItem afv_prompt_library_item：提示词库条目（user_id 为 NULL = 官方）。
type PromptItem struct {
	bun.BaseModel `bun:"table:afv_prompt_library_item,alias:p"`

	ID        int64           `bun:"id,pk,autoincrement"`
	UserID    sql.NullInt64   `bun:"user_id"`
	Title     string          `bun:"title,notnull"`
	Prompt    string          `bun:"prompt,notnull"`
	Vertical  string          `bun:"vertical,notnull"`
	GroupKey  string          `bun:"group_key,notnull"`
	Models    json.RawMessage `bun:"models,type:json,nullzero"`
	Tags      json.RawMessage `bun:"tags,type:json,nullzero"`
	Source    string          `bun:"source,notnull"`
	Enabled   int8            `bun:"enabled,notnull"`
	Sort      int32           `bun:"sort,notnull"`
	Deleted   int8            `bun:"deleted,notnull"`
	CreatedAt time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// 提示词一级分类。
const (
	VerticalEcommerce = "ecommerce"
	VerticalGeneral   = "general"
	VerticalMine      = "mine"
	GroupFavorite     = "favorite" // 用户自己的提示词固定 mine / favorite
)

// Official 是否官方条目。
func (p *PromptItem) Official() bool { return !p.UserID.Valid }
