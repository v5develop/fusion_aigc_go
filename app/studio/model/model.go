// Package model studio 三张表（deploy/sql/Go_V1.0.21__studio.sql，afv_studio_task.color_fix_params 见 Go_V1.0.27）的 bun 结构体。
//
// 按设计方案 §9.5：新模块业务数据访问用 bun，不再用 goctl 生成 model。
// 这里只放结构体与常量；数据访问（插入、CAS 状态迁移、Upsert 缓存、游标分页）在 app/studio/rpc/internal/repo。
//
// JSON 列用 json.RawMessage 承接（type:json 让 bun 按 JSON 文本写入）；空切片配 nullzero 写 NULL。
// 时间列 DATETIME(3)：插入时零值走库默认 CURRENT_TIMESTAMP(3)；查询条件里的时间一律用 FROM_UNIXTIME(ms/1000)
// 交给库按会话时区换算（bundb.New 的方言按 UTC 内联 time.Time，与 DSN 的 +08:00 会话时区不一致）。
//
// 写 Java 共享表（afv_creation_task、afv_asset、afv_billing_*）仍走 pkg/taskcenter、pkg/asset、pkg/billing，不在 bun 里写。
package model

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// Kind 批次 / 任务类型（表里是 ENUM）。
type Kind string

const (
	KindTryon    Kind = "tryon"    // 商拍换装：模特 × 张数，每张一个任务
	KindPose     Kind = "pose"     // 姿态复刻：人物 1 + 姿势 1–9，每个姿势一个任务
	KindImageGen Kind = "imagegen" // 图片生成：count 张，每张一个任务
)

// Valid 是否是已知类型。
func (k Kind) Valid() bool {
	switch k {
	case KindTryon, KindPose, KindImageGen:
		return true
	}
	return false
}

// Kinds 全部类型（固定顺序，任务中心 Source、后台筛选都按它展开）。
var Kinds = []Kind{KindTryon, KindPose, KindImageGen}

// 批次状态（afv_studio_batch.status）。状态机：提交中 → 排队 → 切图中 → 反推中 → 生图中 → 全部成功 / 全部失败 / 部分成功；
// 排队之后的非终态可转已取消（提交中不可取消）。图片生成没有切图与反推（rewrite_prompt 时有一次润色，算反推中）；姿态复刻没有切图。
const (
	BatchQueued     int8 = 0 // 排队：已收单、已冻结点数，等待编排
	BatchCropping   int8 = 1 // 切图中：sidecar crop-plan / media 裁图 / crop-check（仅商拍）
	BatchPrompting  int8 = 2 // 反推中：analyze-batch / pose source+prompt / 提示词润色
	BatchGenerating int8 = 3 // 生图中：子任务已提交 ai-gateway，由轮询器推进
	BatchDone       int8 = 4 // 全部成功
	BatchFailed     int8 = 5 // 全部失败（含切图/反推整批失败，整批退款）
	BatchPartial    int8 = 6 // 部分成功：失败的子任务各自退款
	BatchCanceled   int8 = 7 // 已取消：停止跟踪，未开始的子任务退款（gateway cancel 尽力而为）
	// BatchSubmitting 提交中：批次与子任务已落库、正在逐张冻结点数；全部冻结成功才 CAS → 排队。
	// Poller 的编排不扫它；进程在此状态崩溃时，由 Poller 兜底（created_at 超过 5 分钟）退回已冻结的点数并置失败。
	BatchSubmitting int8 = 8
)

// BatchFinal 批次是否已进终态（终态才允许删除）。
func BatchFinal(s int8) bool {
	return s == BatchDone || s == BatchFailed || s == BatchPartial || s == BatchCanceled
}

// 子任务状态（afv_studio_task.status）。
const (
	TaskQueued    int8 = 0 // 排队：等反推完成 / 等并发名额
	TaskRunning   int8 = 1 // 生成中：已提交 ai-gateway
	TaskSucceeded int8 = 2 // 成功：已转存、已结算、已入素材库
	TaskFailed    int8 = 3 // 失败：已退款
	TaskCanceled  int8 = 4 // 已取消：已退款
	// TaskDelivering 交付中：已出图并转存，正在确认扣费（Settle）。由 生成中 CAS 进入，抢到才 Settle；
	// 强制失败 / 超时失败只从 排队 / 生成中 CAS，交付中不会被改失败，避免「已扣费却标失败」。
	TaskDelivering int8 = 5
)

// TaskActive 子任务是否在途（排队 / 生成中 / 交付中）。
func TaskActive(s int8) bool { return s == TaskQueued || s == TaskRunning || s == TaskDelivering }

// TaskFinal 子任务是否已进终态。
func TaskFinal(s int8) bool { return s == TaskSucceeded || s == TaskFailed || s == TaskCanceled }

// 子任务计费状态（afv_studio_task.billing_status）。权威状态在 Java 预留单表，这里只是本地镜像，便于后台筛选；
// 是否退款一律按 billing_request_id 非空且 Billing.Mode=on 判断，不依赖这个镜像。
const (
	BillingNone     int8 = 0 // 未计费（Billing.Mode=off 或尚未冻结）
	BillingHeld     int8 = 1 // 已冻结
	BillingSettled  int8 = 2 // 已结算
	BillingRefunded int8 = 3 // 已退回
	BillingShadow   int8 = 4 // 影子模式：只算价不动账
)

// 子任务号前缀默认值（studio-rpc TaskIDPrefix 的默认值；admin-api 按 origin_ref 前缀识别 studio 任务时共用）。
const (
	DefaultPrefixTryon    = "tyo"
	DefaultPrefixPose     = "pse"
	DefaultPrefixImageGen = "simg"
)

// IsDefaultPrefix 是否 studio 的子任务号前缀（默认值）。
func IsDefaultPrefix(p string) bool {
	return p == DefaultPrefixTryon || p == DefaultPrefixPose || p == DefaultPrefixImageGen
}

// 缓存类型（afv_studio_prompt_cache.kind）。
const (
	CacheAnalyze  = "analyze"   // 商拍反推：每个模特一次（同模特多张共用 prompt），cache_key=模特图 sha1
	CacheSource   = "source"    // 姿态人物解析：按人物图 sha1 一次
	CachePrompt   = "prompt"    // 姿态提示词：按姿势图 sha1
	CacheCropPlan = "crop_plan" // 切图方案：每批一次（分批调用时 cache_key 带批次号）
)

// Batch afv_studio_batch：一次提交。
type Batch struct {
	bun.BaseModel `bun:"table:afv_studio_batch,alias:b"`

	ID              int64           `bun:"id,pk,autoincrement"`
	UserID          int64           `bun:"user_id,notnull"`
	WorkspaceID     int64           `bun:"workspace_id,notnull"`
	Kind            Kind            `bun:"kind,notnull"`
	RequestID       string          `bun:"request_id,notnull"`
	Payload         json.RawMessage `bun:"payload,type:json,notnull"`
	PayloadHash     string          `bun:"payload_hash,notnull"`
	SourceSha1      sql.NullString  `bun:"source_sha1"`
	EstimatedPoints int64           `bun:"estimated_points,notnull"`
	Status          int8            `bun:"status,notnull"`
	CropPlan        json.RawMessage `bun:"crop_plan,type:json,nullzero"`
	CropCheck       json.RawMessage `bun:"crop_check,type:json,nullzero"`
	SelectedImages  json.RawMessage `bun:"selected_images,type:json,nullzero"`
	StepLog         json.RawMessage `bun:"step_log,type:json,nullzero"`
	TaskTotal       int32           `bun:"task_total,notnull"`
	TaskDone        int32           `bun:"task_done,notnull"`
	TaskFailed      int32           `bun:"task_failed,notnull"`
	ErrorReason     sql.NullString  `bun:"error_reason"`
	Deleted         int8            `bun:"deleted,notnull"`
	CreatedAt       time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt       time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// Task afv_studio_task：一张图。
type Task struct {
	bun.BaseModel `bun:"table:afv_studio_task,alias:t"`

	ID               int64           `bun:"id,pk,autoincrement"`
	BatchID          int64           `bun:"batch_id,notnull"`
	UserID           int64           `bun:"user_id,notnull"`
	Kind             Kind            `bun:"kind,notnull"`
	ModelIndex       int32           `bun:"model_index,notnull"`
	ShotIndex        int32           `bun:"shot_index,notnull"`
	PoseIndex        int32           `bun:"pose_index,notnull"`
	TaskID           sql.NullString  `bun:"task_id"`
	Attempt          int32           `bun:"attempt,notnull"`
	ModelGroup       string          `bun:"model_group,notnull"`
	InputImages      json.RawMessage `bun:"input_images,type:json,nullzero"`
	PromptText       sql.NullString  `bun:"prompt_text"`
	PromptMeta       json.RawMessage `bun:"prompt_meta,type:json,nullzero"`
	GatewayCallID    sql.NullString  `bun:"gateway_call_id"`
	GatewayStatus    sql.NullString  `bun:"gateway_status"`
	Status           int8            `bun:"status,notnull"`
	AdminLocked      int8            `bun:"admin_locked,notnull"`
	ResultRid        sql.NullString  `bun:"result_rid"`
	ResultURL        sql.NullString  `bun:"result_url"`
	ResultWidth      int32           `bun:"result_width,notnull"`
	ResultHeight     int32           `bun:"result_height,notnull"`
	CostCNY          string          `bun:"cost_cny,type:decimal(12,6),notnull,default:0"` // DECIMAL 用字符串承接，避免浮点误差；空串插入时写库默认 0
	BillingRequestID sql.NullString  `bun:"billing_request_id"`
	BillingStatus    int8            `bun:"billing_status,notnull"`
	ErrorReason      sql.NullString  `bun:"error_reason"`
	ColorFixParams   json.RawMessage `bun:"color_fix_params,type:json,nullzero"` // 校色参数（Go_V1.0.27），NULL=没保存过
	MaskURL          sql.NullString  `bun:"mask_url"`                            // 服装蒙版（Go_V1.0.28，白色+软 alpha PNG），NULL=没有
	MaskParts        json.RawMessage `bun:"mask_parts,type:json,nullzero"`       // 逐件服装蒙版 [{part, mask_url}]（Go_V1.0.28），NULL=没有
	StartedAt        sql.NullTime    `bun:"started_at"`
	FinishedAt       sql.NullTime    `bun:"finished_at"`
	Deleted          int8            `bun:"deleted,notnull"`
	CreatedAt        time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt        time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// MaskCache afv_studio_mask_cache（Go_V1.0.28）：区域蒙版缓存，唯一键 (url_key, target, parts)。
type MaskCache struct {
	bun.BaseModel `bun:"table:afv_studio_mask_cache,alias:mc"`

	ID            int64          `bun:"id,pk,autoincrement"`
	URLKey        string         `bun:"url_key,notnull"`
	Target        string         `bun:"target,notnull"`
	Parts         string         `bun:"parts,notnull"` // 服装细分部位（固定顺序逗号拼接），空=不细分
	SourceURL     string         `bun:"source_url,notnull"`
	Status        int8           `bun:"status,notnull"`
	MaskRid       sql.NullString `bun:"mask_rid"`
	MaskURL       sql.NullString `bun:"mask_url"`
	PartMasks     sql.NullString `bun:"part_masks"` // 多部位请求的逐件蒙版 JSON [{part, mask_url}]
	Coverage      string         `bun:"coverage,type:decimal(7,6),notnull,default:0"`
	Width         int32          `bun:"width,notnull"`
	Height        int32          `bun:"height,notnull"`
	ModelGroup    string         `bun:"model_group,notnull"`
	GatewayCallID sql.NullString `bun:"gateway_call_id"`
	CostCNY       string         `bun:"cost_cny,type:decimal(12,6),notnull,default:0"`
	UserID        int64          `bun:"user_id,notnull"`
	Hits          int32          `bun:"hits,notnull"`
	CreatedAt     time.Time      `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt     time.Time      `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// 蒙版缓存状态。
const (
	MaskOK       int8 = 1 // 成功，mask_url 有值
	MaskNoRegion int8 = 2 // 没识别到对应区域
)

// PromptCache afv_studio_prompt_cache：批次内反推 / 切图缓存，唯一键 (batch_id, cache_key, kind)。
type PromptCache struct {
	bun.BaseModel `bun:"table:afv_studio_prompt_cache,alias:pc"`

	ID        int64           `bun:"id,pk,autoincrement"`
	BatchID   int64           `bun:"batch_id,notnull"`
	CacheKey  string          `bun:"cache_key,notnull"`
	Kind      string          `bun:"kind,notnull"`
	Payload   json.RawMessage `bun:"payload,type:json,notnull"`
	CreatedAt time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
}

// StepLog afv_studio_batch.step_log 的一条（数组元素）。不记 base64、Key、完整 prompt。
type StepLog struct {
	Step      string  `json:"step"`                 // submitted / crop_plan / crop_check / analyze / pose_source / pose_prompt / generate …
	Ms        int64   `json:"ms"`                   // 本步耗时（毫秒）
	Images    int     `json:"images,omitempty"`     // 输入图张数
	Crops     int     `json:"crops,omitempty"`      // 切出的图数
	Degraded  bool    `json:"degraded,omitempty"`   // 反推降级
	CostCNY   float64 `json:"cost_cny,omitempty"`   // 本步上游成本（人民币，gateway cost.amount_cny）
	TaskTotal int     `json:"task_total,omitempty"` // submitted 时的子任务数
	Detail    string  `json:"detail,omitempty"`     // 失败原因等附加信息（短文本）
	At        int64   `json:"at"`                   // 记录时间（unix 毫秒）
}

// SelectedImages afv_studio_batch.selected_images 的一项：某个模特**反推读图**选中的图（≤9，模特在前的展示顺序）。
// 不是生图输入——生图参考图按模型上限另行组装（≤14），实际送生图的图序见 afv_studio_task.input_images。
type SelectedImages struct {
	ModelIndex int          `json:"model_index"`
	Images     []InputImage `json:"images"`
}

// InputImage 送生图的一张图（selected_images.images[] 与 afv_studio_task.input_images[] 共用）。
type InputImage struct {
	ID   string `json:"id,omitempty"` // 编号：g-<item>-<angle>-<seq> / m-<i>
	URL  string `json:"url"`
	Role string `json:"role,omitempty"` // model / garment / pose / source / ref
	Sha1 string `json:"sha1,omitempty"`
	// Sha256 内容 SHA-256（随 mr:// 引用一起发给 gateway，便于命中同一 request_hash）
	Sha256 string `json:"sha256,omitempty"`
}
