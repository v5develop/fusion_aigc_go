package aigateway

import (
	"encoding/json"
	"strings"
	"time"
)

// 调用状态（CallStatus）。终态：SUCCEEDED / FAILED / UNKNOWN / CANCELLED。
const (
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusSucceeded = "SUCCEEDED"
	StatusFailed    = "FAILED"
	// StatusUnknown 供应商侧结果无法确定（可能已扣费）。网关不会重试；重开需人工对账后带 reconciled_not_charged。
	StatusUnknown   = "UNKNOWN"
	StatusCancelled = "CANCELLED"
)

// 产物落桶用途（media_purpose）。缺省为 intermediate。
const (
	MediaPurposeIntermediate = "intermediate"
	MediaPurposeAssetLibrary = "asset_library"
)

// MRScheme 媒体服务资源引用前缀：mr://{rid}。
const MRScheme = "mr://"

// 契约上限。
const (
	MaxWait     = 60 * time.Second
	MaxBatchGet = 200
)

// CallRequest POST /v1/calls 的请求体，按 operation 区分 input 结构。
type CallRequest struct {
	// Operation 操作，如 image.generate、video.generate、chat.completion
	Operation string `json:"operation"`
	// ModelGroup 模型组编码，如 img.gpt-image-2；必须支持 Operation
	ModelGroup string `json:"model_group"`
	// Input 操作入参，结构见契约 operations.py（可传结构体、map 或 json.RawMessage）
	Input any `json:"input"`
	// ModelOptions 模型组专属参数，按模型组的 options_schema 校验
	ModelOptions map[string]any `json:"model_options,omitempty"`
	// Constraints 路由与时限约束
	Constraints *Constraints `json:"constraints,omitempty"`
	// Limits 费用上限
	Limits *Limits `json:"limits,omitempty"`
	// Labels 不透明标签（≤16 对，键 ^[a-z0-9][a-z0-9_.-]{0,62}$），网关按其聚合成本，不参与 request_hash
	Labels map[string]string `json:"labels,omitempty"`
	// MediaPurpose 产物存放：intermediate（默认，私有桶）或 asset_library
	MediaPurpose string `json:"media_purpose,omitempty"`
}

// Constraints 路由与时限约束。deadline_at、max_attempts 不参与 request_hash。
type Constraints struct {
	// DeadlineAt 绝对截止时间（UTC）；为空用模型组默认
	DeadlineAt *time.Time `json:"deadline_at,omitempty"`
	// AllowedDeployments 只允许路由到这些部署
	AllowedDeployments []string `json:"allowed_deployments,omitempty"`
	// MaxAttempts 最多尝试次数（1–32）
	MaxAttempts int `json:"max_attempts,omitempty"`
	// MinOutputs n>1 时至少成功几个才算成功（默认 n）
	MinOutputs int `json:"min_outputs,omitempty"`
}

// Limits 费用上限。
type Limits struct {
	// MaxCost 本次调用供应商总成本上限，单位人民币，十进制字符串（如 "2.50"）；参与 request_hash。
	// 软上限：网关只在发起新尝试前检查，不会中断已发出的尝试
	MaxCost string `json:"max_cost,omitempty"`
}

// MediaRef 输入媒体引用。
type MediaRef struct {
	// URI mr://{rid} 或 https 预签名地址
	URI string `json:"uri"`
	// Sha256 内容摘要；带上后重签的 URL 也能命中同一 request_hash
	Sha256 string `json:"sha256,omitempty"`
	// Mime 类型提示
	Mime string `json:"mime,omitempty"`
}

// Call 调用记录（CallRecord），每次读取都是完整记录。未建模的字段忽略。
type Call struct {
	// ID 网关调用 ID，call_ 开头
	ID string `json:"id"`
	// IdempotencyKey 提交时的幂等键
	IdempotencyKey string `json:"idempotency_key"`
	CallerID       string `json:"caller_id"`
	RequestHash    string `json:"request_hash"`
	Operation      string `json:"operation"`
	ModelGroup     string `json:"model_group"`
	// Status 状态，见 Status* 常量
	Status string `json:"status"`
	// Phase 仅 RUNNING 时有：ROUTING / WAITING_CAPACITY / SENDING / POLLING / RECONCILING / REHOSTING
	Phase string `json:"phase,omitempty"`
	// RunNo 第几轮运行；retry_failed 重开时 +1
	RunNo int `json:"run_no"`
	// UpdatedSeq 单调递增的变更序号，用于比对是否有未处理的变更
	UpdatedSeq int64     `json:"updated_seq"`
	Attempts   []Attempt `json:"attempts"`
	// Result SUCCEEDED 时的产物（FAILED 时也可能带部分 n 结果）
	Result  *Result         `json:"result,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Cost    *Cost           `json:"cost,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
	// RetrySafe 本轮没有任何尝试可能被扣费，可用 retry_failed 重开
	RetrySafe bool `json:"retry_safe"`
	// MoneyAtRisk 存在可能已扣费但未交付结果的尝试
	MoneyAtRisk bool `json:"money_at_risk"`
	// VendorAccepted 供应商当前持有本调用的某次尝试（已受理、可能已扣费）；取消会被拒（CANCEL_NOT_ALLOWED）
	VendorAccepted   bool              `json:"vendor_accepted,omitempty"`
	VendorAcceptedAt *time.Time        `json:"vendor_accepted_at,omitempty"`
	Limits           *Limits           `json:"limits,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	DeadlineAt       *time.Time        `json:"deadline_at,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	CompletedAt      *time.Time        `json:"completed_at,omitempty"`
	// LateSuccess 调用结束后才落下的已扣费结果（状态不变，结果在 LateResult）
	LateSuccess     bool       `json:"late_success,omitempty"`
	LateSuccessAt   *time.Time `json:"late_success_at,omitempty"`
	LateResult      *Result    `json:"late_result,omitempty"`
	LateDeliveredAt *time.Time `json:"late_delivered_at,omitempty"`
}

// IsTerminal 是否已到终态。
func (c *Call) IsTerminal() bool {
	switch c.Status {
	case StatusSucceeded, StatusFailed, StatusUnknown, StatusCancelled:
		return true
	}
	return false
}

// Succeeded 是否成功。
func (c *Call) Succeeded() bool { return c.Status == StatusSucceeded }

// Attempt 一次供应商尝试。
type Attempt struct {
	// Seq 调用内从 1 开始的序号（跨轮次）
	Seq   int `json:"seq"`
	RunNo int `json:"run_no"`
	// Deployment 部署编码
	Deployment string `json:"deployment"`
	// Provider 供应商编码
	Provider string `json:"provider"`
	// State PREPARED / SENDING / ACCEPTED / POLLING / RECONCILING / REHOSTING / SUCCEEDED / FAILED / UNKNOWN
	State string `json:"state"`
	// OutcomeClass 发送结果分类（NOT_SENT / REJECTED / INPUT_REJECTED / CONTENT_POLICY …）
	OutcomeClass    string `json:"outcome_class,omitempty"`
	RejectionReason string `json:"rejection_reason,omitempty"`
	PolicyStage     string `json:"policy_stage,omitempty"`
	// Charged CHARGED / NOT_CHARGED / UNKNOWN
	Charged string `json:"charged"`
	// ProviderRef 供应商侧任务 ID
	ProviderRef string       `json:"provider_ref,omitempty"`
	Cost        *Cost        `json:"cost,omitempty"`
	VendorError *VendorError `json:"vendor_error,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	SentAt      *time.Time   `json:"sent_at,omitempty"`
	AcceptedAt  *time.Time   `json:"accepted_at,omitempty"`
	FinishedAt  *time.Time   `json:"finished_at,omitempty"`
}

// VendorError 脱敏后的供应商错误证据。
type VendorError struct {
	HTTPStatus int    `json:"http_status,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Cost 费用。金额均为十进制字符串。
type Cost struct {
	Amount string `json:"amount"`
	// Currency 三位币种代码
	Currency string `json:"currency"`
	// AmountCNY 折算人民币
	AmountCNY string `json:"amount_cny"`
	// Source configured / billed / estimated
	Source string `json:"source"`
	// Final 为 false 时后续还可能修正
	Final bool `json:"final"`
}

// Failure 调用（或单个产物）失败原因。Code 是稳定的判据码，Message 只给运维看，不进用户文案。
type Failure struct {
	// Class INPUT_REJECTED / CONTENT_POLICY / DEADLINE_EXCEEDED / NO_CAPACITY / NO_ROUTE / ATTEMPTS_EXHAUSTED / VENDOR_FAILED / VENDOR_OUTCOME_UNKNOWN …
	Class   string `json:"class"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// MediaOut 媒体服务已入库的产物。持久引用只有 mr://{rid}；URL 是 24 小时签名下载地址。
type MediaOut struct {
	// Rid 媒体服务资源 ID；本版本结果必有
	Rid string `json:"rid,omitempty"`
	// AigKey 已废弃的别名（本版本等于 rid）
	AigKey string `json:"aig_key,omitempty"`
	// URL 签名下载地址（24 小时）
	URL          string    `json:"url"`
	URLExpiresAt time.Time `json:"url_expires_at"`
	Sha256       string    `json:"sha256"`
	Bytes        int64     `json:"bytes"`
	Mime         string    `json:"mime"`
	Width        int       `json:"width,omitempty"`
	Height       int       `json:"height,omitempty"`
	DurationMs   int64     `json:"duration_ms,omitempty"`
}

// Ref 产物的持久引用 mr://{rid}；旧结果没有 rid 时返回空串。
func (m *MediaOut) Ref() string {
	if m == nil || m.Rid == "" {
		return ""
	}
	return MRScheme + m.Rid
}

// RidOf 从 mr://{rid} 取出 rid；不是 mr:// 引用返回空串。
func RidOf(ref string) string {
	if rid, ok := strings.CutPrefix(ref, MRScheme); ok {
		return rid
	}
	return ""
}

// OutputItem image.generate / image.edit 的单个产物，各自成功或失败。
type OutputItem struct {
	Index int `json:"index"`
	// Status SUCCEEDED / FAILED
	Status        string    `json:"status"`
	Media         *MediaOut `json:"media,omitempty"`
	Failure       *Failure  `json:"failure,omitempty"`
	RevisedPrompt string    `json:"revised_prompt,omitempty"`
}

// Result 操作产物。结构随 operation 变化：这里只解出常用的媒体字段，完整内容在 Raw，可用 Decode 解到自定义结构。
type Result struct {
	Operation string `json:"operation"`
	// Outputs image.generate / image.edit
	Outputs []OutputItem `json:"outputs,omitempty"`
	// Image / Mask image.segment
	Image *MediaOut `json:"image,omitempty"`
	Mask  *MediaOut `json:"mask,omitempty"`
	// Parts image.segment 按 input.classes 分割时的逐类抠图（请求顺序；图里没有的类缺省）
	Parts []SegmentPart `json:"parts,omitempty"`
	// Preview image.layer
	Preview *MediaOut `json:"preview,omitempty"`
	// Video / Cover / LastFrame video.generate、video.enhance、video.subtitle_erase
	Video     *MediaOut `json:"video,omitempty"`
	Cover     *MediaOut `json:"cover,omitempty"`
	LastFrame *MediaOut `json:"last_frame,omitempty"`
	// Audio tts.synthesize
	Audio *MediaOut `json:"audio,omitempty"`

	// Raw 原始 JSON
	Raw json.RawMessage `json:"-"`
}

// SegmentPart image.segment 的一类抠图。
type SegmentPart struct {
	// Label 请求的类名（如 tops / coat / pants）
	Label string    `json:"label"`
	Image *MediaOut `json:"image,omitempty"`
}

// UnmarshalJSON 解出常用字段并保留原文。
func (r *Result) UnmarshalJSON(b []byte) error {
	type plain Result
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = Result(p)
	r.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON 原样输出 Raw（便于落库后再解析）。
func (r Result) MarshalJSON() ([]byte, error) {
	if len(r.Raw) > 0 {
		return r.Raw, nil
	}
	type plain Result
	return json.Marshal(plain(r))
}

// Decode 把原始产物解到 v（如文本类操作的自定义结构）。
func (r *Result) Decode(v any) error { return json.Unmarshal(r.Raw, v) }

// Media 主要媒体产物：成功的 outputs[].media，以及 image / video / audio / preview；不含 mask、cover、last_frame 等附属产物。
func (r *Result) Media() []MediaOut {
	if r == nil {
		return nil
	}
	var out []MediaOut
	for _, o := range r.Outputs {
		if o.Status == StatusSucceeded && o.Media != nil {
			out = append(out, *o.Media)
		}
	}
	for _, m := range []*MediaOut{r.Image, r.Video, r.Audio, r.Preview} {
		if m != nil {
			out = append(out, *m)
		}
	}
	return out
}

// ModelGroup 模型组（ModelGroupInfo）。
type ModelGroup struct {
	Code string `json:"code"`
	// Operations 支持的操作
	Operations  []string `json:"operations"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
	// OptionsSchema model_options 需满足的 JSON Schema
	OptionsSchema json.RawMessage `json:"options_schema,omitempty"`
	// FeatureLimits 启用成员能力的并集
	FeatureLimits FeatureLimits `json:"feature_limits"`
	// DefaultTimeoutS 未传 deadline_at 时的默认时限（秒）
	DefaultTimeoutS int `json:"default_timeout_s,omitempty"`
}

// FeatureLimits 模型组可服务的请求特性，字段缺省表示不限。
type FeatureLimits struct {
	MaxN                 *int     `json:"max_n,omitempty"`
	MaxReferenceImages   *int     `json:"max_reference_images,omitempty"`
	MaxReferenceVideos   *int     `json:"max_reference_videos,omitempty"`
	MaxReferenceAudios   *int     `json:"max_reference_audios,omitempty"`
	SupportsMask         *bool    `json:"supports_mask,omitempty"`
	SupportsFirstFrame   *bool    `json:"supports_first_frame,omitempty"`
	SupportsLastFrame    *bool    `json:"supports_last_frame,omitempty"`
	Sizes                []string `json:"sizes,omitempty"`
	AspectRatios         []string `json:"aspect_ratios,omitempty"`
	Resolutions          []string `json:"resolutions,omitempty"`
	DurationsS           []int    `json:"durations_s,omitempty"`
	MaxDurationS         *int     `json:"max_duration_s,omitempty"`
	MaxInputBytes        *int64   `json:"max_input_bytes,omitempty"`
	MaxInputPixels       *int64   `json:"max_input_pixels,omitempty"`
	InputMimeTypes       []string `json:"input_mime_types,omitempty"`
	SupportsTools        *bool    `json:"supports_tools,omitempty"`
	SupportsJSONResponse *bool    `json:"supports_json_response,omitempty"`
	SupportsThinking     *bool    `json:"supports_thinking,omitempty"`
	SupportsVision       *bool    `json:"supports_vision,omitempty"`
	SupportsVideoInput   *bool    `json:"supports_video_input,omitempty"`
	MaxContextTokens     *int64   `json:"max_context_tokens,omitempty"`
}

// ReadinessItem 模型组当前是否有可路由的部署（调用方缓存约 10 秒）。
type ReadinessItem struct {
	ModelGroup string `json:"model_group"`
	// Operation 请求里带了 operation 时回显
	Operation string `json:"operation,omitempty"`
	Ready     bool   `json:"ready"`
	// Reason 不可用的判据：model_group.unknown / model_group.disabled / operation.mismatch / deployments.none_enabled /
	// deployments.all_circuit_open / deployments.all_balance_excluded / runtime.unavailable …
	Reason               string `json:"reason,omitempty"`
	EnabledDeployments   int    `json:"enabled_deployments"`
	AvailableDeployments int    `json:"available_deployments"`
}

// CostsQuery GET /v1/ledger/costs 的游标查询。
type CostsQuery struct {
	// AfterID 游标，首次传 0
	AfterID int64
	// Limit 每页条数（1–1000，0 用服务端默认 500）
	Limit int
}

// CostLedgerPage 成本流水一页（按 id 升序）。
type CostLedgerPage struct {
	Items []CostLedgerRow `json:"items"`
	// NextAfterID 下一页的 after_id
	NextAfterID int64 `json:"next_after_id"`
	HasMore     bool  `json:"has_more"`
}

// CostLedgerRow 一条成本事实。同一 attempt 的修正是新行（revision 更大），按 attempt_id 取最大 revision。
type CostLedgerRow struct {
	ID             int64             `json:"id"`
	AttemptID      int64             `json:"attempt_id"`
	Revision       int               `json:"revision"`
	CallID         string            `json:"call_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	CallerID       string            `json:"caller_id"`
	Labels         map[string]string `json:"labels,omitempty"`
	Operation      string            `json:"operation"`
	ModelGroup     string            `json:"model_group"`
	Deployment     string            `json:"deployment"`
	Provider       string            `json:"provider"`
	Amount         string            `json:"amount"`
	Currency       string            `json:"currency"`
	AmountCNY      string            `json:"amount_cny"`
	Source         string            `json:"source"`
	Final          bool              `json:"final"`
	RecordedAt     time.Time         `json:"recorded_at"`
}
