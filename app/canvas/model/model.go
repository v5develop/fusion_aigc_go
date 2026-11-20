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
	RunSucceeded int8 = 2 // 成功：已结算；产物可能仍是私有引用 mr://（后台转存素材库，见 CopyJob），转存后换成素材库地址并入库
	RunFailed    int8 = 3 // 失败：gateway 后端已退款
	RunCanceled  int8 = 4 // 已取消：gateway 后端已退款
	// RunSubmitting 提交中，两个阶段（按 attempt 区分）：
	//   - attempt=0：已落库（task_id / billing_request_id 已生成），正在冻结点数；冻结成功后一次 CAS 转排队。
	//   - attempt>0：派发器已用 CAS 从排队抢到（条件含「未请求取消」，同时 attempt+1），正在向后端发请求；
	//     成功转运行中，暂时性错误退回排队，永久错误确认网关无已发出调用后失败退款。
	// 轮询器的排队 / 运行中扫描都不看它，只做兜底（见 logic.advanceSubmitting）。
	RunSubmitting int8 = 5
)

// 用户取消请求（afv_canvas_node_run.cancel_request，Go_V1.0.61）。退款只有一个主人，且只在确认没发给供应商之后退：
// 排队 / 提交中的取消只打标记，由持有 run 的一方（派发器 / 排队推进 / 提交中兜底）确认网关无已发出调用后结束并退款。
const (
	CancelNone      int8 = 0 // 没有
	CancelRequested int8 = 1 // 已请求取消，待确认（接口 cancel_state=requested，前端「取消中」）；只出现在排队 / 提交中
	CancelRefused   int8 = 2 // 取消被拒：调用已发给供应商，run 照常完成、按实际结算（接口 cancel_state=refused）
)

// CancelStateText 取消请求 → 接口字面量（空 = 没有）。
func CancelStateText(v int8) string {
	switch v {
	case CancelRequested:
		return "requested"
	case CancelRefused:
		return "refused"
	}
	return ""
}

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

// Candidate afv_canvas_node_run.candidates 的一项。RID 为媒体服务资源，URL 为长期可读地址：素材库桶的公开地址，
// 或转存完成前的私有引用 mr://{rid}.{ext}（读时现签，见 logic/media_ref.go；RID 此时是私有源 rid）。
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
	CancelRequest    int8            `bun:"cancel_request,notnull"` // 用户取消请求（CancelNone / CancelRequested / CancelRefused，Go_V1.0.61）
	Deleted          int8            `bun:"deleted,notnull"`
	CreatedAt        time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	StartedAt        sql.NullTime    `bun:"started_at"`
	FinishedAt       sql.NullTime    `bun:"finished_at"`
	UpdatedAt        time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`

	// LibraryState 读出口算出的 library_status（pending / failed / 空），不落库：引用被换成现签地址之前算好带出来。
	LibraryState string `bun:"-"`
	// Thumbs 读出口算出的候选缩略图现签地址（与 CandidateList 同序，"" = 没有），不落库；只有私有引用的图片候选才有。
	Thumbs []string `bun:"-"`
}

// CandidateList 解出候选（解析失败按空）。
func (r *NodeRun) CandidateList() []Candidate {
	var out []Candidate
	if len(r.Candidates) > 0 {
		_ = json.Unmarshal(r.Candidates, &out)
	}
	return out
}

// 转存 outbox 状态（afv_canvas_copy_job.status）。
const (
	CopyPending int8 = 0 // 待转存
	CopyDone    int8 = 1 // 已完成：run 的引用已换成素材库地址，已入素材库
	CopyFailed  int8 = 2 // 永久失败：产物仍以 mr:// 可看可下载，run.error_code=CopyFailedCode
	CopyVoid    int8 = 3 // 作废：run 已不是成功态 / 已删除
)

// CopyFailedCode 转存永久失败时写在**成功** run 上的 error_code 标记（成功 run 的 error_code 不对外当失败展示，
// 只用于 library_status=failed 与运营排查）。
const CopyFailedCode = "LIBRARY_COPY_FAILED"

// CopyJob afv_canvas_copy_job：一个成功 run 的产物转存素材库任务（Go_V1.0.29）。
type CopyJob struct {
	bun.BaseModel `bun:"table:afv_canvas_copy_job,alias:j"`

	ID         int64          `bun:"id,pk,autoincrement"`
	RunPK      int64          `bun:"run_pk,notnull"`
	TaskID     string         `bun:"task_id,notnull"`
	UserID     int64          `bun:"user_id,notnull"`
	Status     int8           `bun:"status,notnull"`
	Attempt    int32          `bun:"attempt,notnull"`
	NextAt     time.Time      `bun:"next_at,notnull"`
	LastError  sql.NullString `bun:"last_error"`
	CreatedAt  time.Time      `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt  time.Time      `bun:"updated_at,notnull,default:current_timestamp(3)"`
	FinishedAt sql.NullTime   `bun:"finished_at"`
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
	GroupMine         = "mine"
)

// PromptGroupOrder 二级分类的固定展示顺序与中文名（与原型一致；未登记的 key 排在末尾，名称回退为 key）。
var PromptGroupOrder = []struct{ Key, Label string }{
	{"model", "模特 / 人像"}, {"scene", "场景"}, {"product", "商品图 / 主图"}, {"detail", "详情页"},
	{"apparel", "服装"}, {"photo", "摄影 / 光线"}, {"composition", "构图 / 镜头"}, {"style", "风格 / 艺术"},
	{"material", "材质 / 质感"}, {"text", "文字 / 排版"}, {GroupMine, "我的"}, {GroupFavorite, "我的收藏"},
}

// PromptGroupLabel 二级分类中文名；未登记返回 key 本身。
func PromptGroupLabel(key string) string {
	for _, g := range PromptGroupOrder {
		if g.Key == key {
			return g.Label
		}
	}
	return key
}

// PromptGroupRank 展示顺序，未登记的排在最后。
func PromptGroupRank(key string) int {
	for i, g := range PromptGroupOrder {
		if g.Key == key {
			return i
		}
	}
	return len(PromptGroupOrder)
}

// Official 是否官方条目。
func (p *PromptItem) Official() bool { return !p.UserID.Valid }
