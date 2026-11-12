package pipeline

import (
	"context"
	"errors"
	"net"
	"time"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/reverse"
)

// ---------------------------------------------------------------- 外部依赖（小接口）

// Media 媒体服务的子集；*mediasvc.Client 满足。
type Media interface {
	IngestURL(ctx context.Context, srcURL, purpose, idem string) (mediasvc.Resource, error)
	Conform(ctx context.Context, rid string, p mediasvc.ConformParams, idem string) (mediasvc.Resource, error)
	Crop(ctx context.Context, rid string, boxes []mediasvc.PixelBox, p mediasvc.CropParams, idem string) ([]mediasvc.Resource, error)
	Grid(ctx context.Context, rids []string, p mediasvc.GridParams, idem string) (mediasvc.Resource, error)
	FetchBytes(ctx context.Context, rid string) ([]byte, string, error)
	ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error)
	CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error)
}

// Reverse 反推 sidecar；*reverse.Client 满足。
type Reverse interface {
	Status(ctx context.Context) (*reverse.Status, error)
	// garments 可选（≤6 张 data URL）：单人原图同时读服装参考图，见 reverse.Client.PoseSource
	PoseSource(ctx context.Context, imageDataURL string, garments ...string) (*reverse.PoseSource, error)
	// mapping 多人配对（nil = sidecar 默认配对），见 reverse.PosePromptReq.Mapping
	PosePrompt(ctx context.Context, source *reverse.PoseSource, poseDataURL, notes string, mapping []int) (*reverse.PosePrompt, error)
	Analyze(ctx context.Context, req reverse.AnalyzeReq) (*reverse.AnalyzeResp, error)
	AnalyzeBatch(ctx context.Context, req reverse.AnalyzeBatchReq) (*reverse.AnalyzeBatchResp, error)
	CropPlan(ctx context.Context, req reverse.CropPlanReq) (*reverse.CropPlanResp, error)
	CropCheck(ctx context.Context, items []reverse.CropCheckItem) ([]reverse.CropCheckResult, error)
}

// Gateway ai-gateway；*aigateway.Client 满足。
type Gateway interface {
	Submit(ctx context.Context, req aigateway.CallRequest, opt aigateway.SubmitOptions) (*aigateway.Call, error)
	Get(ctx context.Context, id string, wait time.Duration) (*aigateway.Call, error)
	BatchGet(ctx context.Context, ids []string) ([]aigateway.Call, error)
	Cancel(ctx context.Context, id string) (*aigateway.Call, error)
}

var (
	_ Media   = (*mediasvc.Client)(nil)
	_ Reverse = (*reverse.Client)(nil)
	_ Gateway = (*aigateway.Client)(nil)
)

// ---------------------------------------------------------------- 时限与重试

// 各步超时（s1.1：切图 / 反推 120s，生图 300s）。注意 sidecar README 建议 HTTP 超时 ≥200s（读图单次 180s），
// 这里是整步的业务时限，由 Poller 用 context.WithTimeout 套在步骤外。
const (
	CropTimeout     = 120 * time.Second
	AnalyzeTimeout  = 120 * time.Second
	GenerateTimeout = 300 * time.Second
)

// GenerateBackoff 生图重试间隔（共 3 次）：只对 429 / 5xx / 超时。
var GenerateBackoff = []time.Duration{1200 * time.Millisecond, 2400 * time.Millisecond, 4800 * time.Millisecond}

// GenerateRetryDelay 第 attempt 次（从 0 起）失败后的等待；超过次数返回 false。
func GenerateRetryDelay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= len(GenerateBackoff) {
		return 0, false
	}
	return GenerateBackoff[attempt], true
}

// RetryableSubmitErr 提交 / 查询 gateway 的错误是否值得重试：429、5xx、网络与超时。
// 409（幂等冲突）、401/403、其余 4xx、调用方取消一律不重试。
func RetryableSubmitErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ge *aigateway.Error
	if errors.As(err, &ge) {
		return ge.HTTPStatus == 0 || ge.HTTPStatus == 429 || ge.HTTPStatus >= 500
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// 可重试的终态失败类（对应 429 / 5xx / 超时）；INPUT_REJECTED、CONTENT_POLICY、NO_ROUTE、UNKNOWN 不重试。
var retryableFailure = map[string]bool{
	"DEADLINE_EXCEEDED":  true, // 超时
	"NO_CAPACITY":        true, // 限流 / 容量
	"ATTEMPTS_EXHAUSTED": true, // 各部署都 429 / 未发出
	"VENDOR_FAILED":      true, // 厂商 5xx
}

// RetryableCall 终态调用是否值得用 retry_failed 重开：必须 FAILED 且 retry_safe，且失败类属于 429/5xx/超时。
func RetryableCall(call *aigateway.Call) bool {
	if call == nil || call.Status != aigateway.StatusFailed || !call.RetrySafe || call.Failure == nil {
		return false
	}
	return retryableFailure[call.Failure.Class]
}

// ---------------------------------------------------------------- 步骤

// 步骤名（日志 step 字段）。
const (
	StepIngest    = "ingest"
	StepConform   = "conform"
	StepCropPlan  = "crop_plan"
	StepCrop      = "crop"
	StepCropCheck = "crop_check"
	StepSelect    = "select"
	StepAnalyze   = "analyze"
	StepSource    = "pose_source"
	StepPrompt    = "pose_prompt"
	StepGenerate  = "generate"
	StepTransfer  = "transfer"
)

// 批次状态（与 pb Batch.status 一致：0 排队 1 切图中 2 反推中 3 生图中）。
const (
	StageQueued     = 0
	StageCropping   = 1
	StagePrompting  = 2
	StageGenerating = 3
)

// StageOf 步骤所属批次阶段。
func StageOf(step string) int {
	switch step {
	case StepIngest, StepConform, StepCropPlan, StepCrop, StepCropCheck, StepSelect:
		return StageCropping
	case StepAnalyze, StepSource, StepPrompt:
		return StagePrompting
	case StepGenerate, StepTransfer:
		return StageGenerating
	}
	return StageQueued
}

// StepLog 每步一行 JSON 日志（验收 I）。不记 base64、Key、完整 prompt。
type StepLog struct {
	JobID    string   `json:"job_id,omitempty"` // 批次 id 或子任务号，Plan.Run 统一补
	Step     string   `json:"step"`
	Ms       int64    `json:"ms"`
	Images   int      `json:"images"`
	Crops    int      `json:"crops"`
	Degraded bool     `json:"degraded"`
	CostCNY  float64  `json:"cost_cny"`
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// Step 一个编排步骤。
type Step interface {
	Name() string
	Run(ctx context.Context) (StepLog, error)
}

// StepFunc 用函数实现 Step。
type StepFunc struct {
	StepName string
	Fn       func(ctx context.Context) (StepLog, error)
}

func (s StepFunc) Name() string                             { return s.StepName }
func (s StepFunc) Run(ctx context.Context) (StepLog, error) { return s.Fn(ctx) }

// Plan 顺序执行的步骤。
type Plan struct {
	JobID string
	Steps []Step
	// OnLog 每步结束（无论成败）回调一次，用于写日志 / 推进批次状态；可空。
	OnLog func(StepLog)
}

// Run 依次执行，遇错即停。StepLog 的 Step / Ms / JobID 缺省时自动补；返回已执行步骤的日志。
func (p Plan) Run(ctx context.Context) ([]StepLog, error) {
	logs := make([]StepLog, 0, len(p.Steps))
	for _, s := range p.Steps {
		if err := ctx.Err(); err != nil {
			return logs, err
		}
		start := time.Now()
		lg, err := s.Run(ctx)
		if lg.Step == "" {
			lg.Step = s.Name()
		}
		if lg.Ms == 0 {
			lg.Ms = time.Since(start).Milliseconds()
		}
		if lg.JobID == "" {
			lg.JobID = p.JobID
		}
		if err != nil && lg.Error == "" {
			lg.Error = err.Error()
		}
		logs = append(logs, lg)
		if p.OnLog != nil {
			p.OnLog(lg)
		}
		if err != nil {
			return logs, err
		}
	}
	return logs, nil
}
