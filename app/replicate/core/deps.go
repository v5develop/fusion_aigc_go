// Package core 视频做同款（video-replicate）的业务内核：视频分析、方案卡与分段报价、确认与分段预留、
// 出片步骤状态机（自动重试 1 次、单段重做、失败退点）、先出第 1 段再继续的决策卡。
//
// 依赖方向（DESIGN §5.1 / §10.10）：只 Go → Java 共享表，经 pkg/billing 动账；不调任何 Java HTTP。
// 外部依赖全部收在本文件的接口里（Store / Ledger / Gateway / Media / Cutter），由 rpc 服务装配；
// 尚不存在的基础设施（媒体服务按区间切段 /video/cut、拼接 /video/stitch 等）以接口占位，未装配时对应能力明确关闭，不伪造。
package core

import (
	"context"
	"errors"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/analysisfix"
	"aigc-local-life/app/replicate/rules/segment"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
)

// EntryCode 计费入口码（afv_billing_offer.entry_code，需运营按模型组配置按秒档位，DESIGN §9.3）。
const EntryCode = "video_replicate"

// Config 运行参数（上线后由 afv_system_config 的 replicate.* 覆盖，见 DESIGN §14.2；本切片只做静态配置）。
type Config struct {
	EntryCode         string
	Limits            segment.Limits
	MaxInFlight       int           // 每 job 同时在途的段数（默认 3）
	AutoRetryMax      int           // 瞬时错误自动重试次数（默认 1；0=关闭）
	RedoMaxPerSegment int           // 每段用户重做上限（默认 3）
	RedoMaxPerJob     int           // 每 job 用户重做总上限（默认 10）
	ContinueWindow    time.Duration // 待继续窗口（默认 24 小时）
	// 待决策提醒（notify.go remindDecision，CONTRACT §O2）：待继续离到期还剩 DecisionReminderLead（默认 4 小时）时提醒一次；
	// DecisionReminderOff 关闭。Replicate.DecisionReminder.*，运行时 afv_system_config.replicate.decision_reminder.*。
	DecisionReminderOff  bool
	DecisionReminderLead time.Duration
	PlanTTL              time.Duration // 方案有效期（默认 7 天）
	QuoteTTL             time.Duration // 报价有效期（默认 10 分钟）
	SegmentDeadline      time.Duration // 每段网关时限（默认 25 分钟，文案「每一段最长等 25 分钟」）
	ReapAfter            time.Duration // 提交中 / 继续中崩溃回收阈值（默认 5 分钟）
	VisionModelGroup     string        // 画面理解（chat.completion，vlm.*）
	// Models 开放的出片模型组（空 = 目录里全部，见 models.go）；DefaultModel 推荐模型（空 / 未开放 = 第一个开放的）。
	Models       []string
	DefaultModel string
	// WriterModelGroup 写手（chat.completion）模型组；空 = 不建写手步骤，出片直接用方言模板（writer.go）。
	WriterModelGroup string
	// WriterMaxAttempts 写手最多写几次（不合格把问题回灌重写，默认 2，上限 3）；WriterDeadline 写手步骤总时限（默认 3 分钟），超时回落模板。
	WriterMaxAttempts int
	WriterDeadline    time.Duration
	ASRModelGroup     string        // 台词（asr.transcribe）
	MaxCostCNY        string        // 单次网关调用成本上限（limits.max_cost）
	FrameCount        int           // 抽帧数（默认 9：一张 3×3 联系表）
	PollWait          time.Duration // 推进时对在途网关调用的长轮询（默认 0：立即返回，由 Poller 周期驱动）
	// ETA 冷启动先验：每「轮」（同时在途 ≤ MaxInFlight 段）的 p50 / p90 耗时（目标假设，默认 4 / 6 分钟）。
	// 装配了历史校准（WithEtaCalibrator）且该桶样本足够时用历史 P50 / P90 替换（eta.go），否则回落这里（prior=true）。
	EtaRoundP50 time.Duration
	EtaRoundP90 time.Duration
	// AnalysisRules 分析结果纠偏规则配置（rules/analysisfix；afv_system_config.replicate.analysis_rules）。
	// 零值 = 全部规则开、默认阈值。读取分析结果时套用（不进分析缓存键，改了不让已有分析失效）。
	AnalysisRules analysisfix.Config
	// ConfirmThresholdPoints 发送前二次确认阈值（afv_system_config.replicate.confirm_threshold_points，默认 0 = 不启用）：
	// 方案总价（全部段合计）超过它时，确认必须带 expectedPoints 且与现算一致，否则 409 QUOTE_CHANGED。
	ConfirmThresholdPoints int64
	// FontRid 成片叠字与失败段占位用的中文字体（媒体服务 font 摄入得到的 rid；afv_system_config.replicate.font_rid）。
	// 空 = 占位用英文文案、不叠字（日志 WARN）。
	FontRid string
	// PlaceholderText 失败段占位画面上的文字（默认「这一段还在重做」）。
	PlaceholderText string
	// KeyPrefix 网关 / 媒体服务幂等键的环境前缀（Replicate.KeyPrefix，默认空 = 与旧键一致）。
	// 只允许 [A-Za-z0-9._-]、≤32 字符；确认时冻结进 job.input、分析开始时冻结进 analysis.state，之后改配置不影响在途键。
	KeyPrefix string
	// HoldTTL 视频预留单的初始有效期（pkg/billing Hold.VideoMinutes，默认 200 分钟；到期被 Reclaimer 回收）。
	HoldTTL time.Duration
	// RenewAfter 预留单冻结（镜像写成已预留）多久后开始逐轮续期。billing.Renew 把到期设成「现在 + billing.LeaseDuration（15 分钟）」，
	// 早续会把剩余时长缩短；所以强制 RenewAfter ≥ HoldTTL − LeaseDuration（默认再晚 1 分钟吸收时钟误差）：
	// 任何一次续期的新到期时间都不早于原到期时间。
	RenewAfter time.Duration
	// ActiveRepoll 在途 job / 分析每轮推进后下次再推的间隔（默认 3 秒；写 next_run_at，到期的按它升序取，防饿死）。
	ActiveRepoll time.Duration
	// SettleTimeout 退点 / 确认扣除的单次时限（脱离请求上下文执行，客户端断开也做完；默认 15 秒）。
	SettleTimeout time.Duration
	// URLTTL 任务视图里播放 / 下载地址的签名有效期（默认 1 小时，上限 24 小时；过期后前端重拉 job 即可）。
	URLTTL time.Duration

	// 方案确认前的内容审核（moderation.go；零值即 Java InputModerationService 的默认策略）：
	// ModerationOff 关闭审核（afv_system_config.replicate.moderation.enabled=false）；ModerationFailOpen 审核服务故障时放行
	// （默认 fail-close：不让确认）；ModerationReviewPass 供应商「嫌疑」放行（默认按违规）。
	ModerationOff        bool
	ModerationFailOpen   bool
	ModerationReviewPass bool
	// ModerationTextGroup / ModerationImageGroup 网关审核模型组（默认 moderation.text.default / moderation.image.default）；
	// ModerationTextDeadline / ModerationImageDeadline 每类整体截止（默认 5 秒 / 12 秒）。
	ModerationTextGroup     string
	ModerationImageGroup    string
	ModerationTextDeadline  time.Duration
	ModerationImageDeadline time.Duration
	// ModerationRejectTTL 不通过的结论多久后允许重审（默认 24 小时；afv_system_config.replicate.moderation.reject_ttl_hours）
	ModerationRejectTTL time.Duration
	// ModerationConfirmWait 确认时还没有有效结论，最多等后台预审多久（默认 3 秒；afv_system_config.replicate.moderation.confirm_wait_ms）。
	// 等不到 → MODERATION_PENDING「内容检查还没完成，几秒后再点确认」，不建 job、不预留点数。
	ModerationConfirmWait time.Duration
	// ModerationPlanWait 生成 / 报价方案时等后台预审的时长（默认 0：不等，方案立即返回、审核状态 pending；
	// afv_system_config.replicate.moderation.plan_wait_ms，0~5000）。结论出得快时可调大，让方案卡直接显示结论。
	ModerationPlanWait time.Duration

	// 定妆图（preview.go；Replicate.Preview.*）：PreviewOff 关闭；PreviewModelGroup 出图的网关模型组（image.edit，默认与 studio 通用档一致
	// img.gpt-image-2.5-sunburst）；PreviewDailyQuota 每人每个自然日（Asia/Shanghai）免费次数（默认 3）；PreviewWait 等网关出图的上限（默认 60 秒）。
	PreviewOff        bool
	PreviewModelGroup string
	PreviewDailyQuota int
	PreviewWait       time.Duration

	// 发布文案（publish_copy.go；Replicate.Copy.*，运行时 afv_system_config.replicate.copy.*）：CopyOff 关闭；CopyModelGroup 写文案的网关 chat 模型组
	// （空 = 回落写手模型组，再回落画面理解模型组）；CopyDailyQuota 每人每个自然日（Asia/Shanghai）次数（默认 20）；CopyWait 等网关的上限（默认 60 秒）。
	CopyOff        bool
	CopyModelGroup string
	CopyDailyQuota int
	CopyWait       time.Duration

	// 坏片判定（badoutput.go，CONTRACT §J4；Replicate.BadOutput.*，运行时 afv_system_config.replicate.bad_output.*）：
	// 段视频落桶后、确认扣点前调媒体服务 video/inspect。BadOutputOff 关闭；黑屏占时长比例 ≥ BadOutputBlackRatio（默认 0.6）
	// 或定格占比 ≥ BadOutputFreezeRatio（默认 0.8）或解不开 → 判坏片（BAD_OUTPUT：退点、不自动重试）。检测器自身故障 / 没装配媒体服务 → 放行。
	BadOutputOff         bool
	BadOutputBlackRatio  float64
	BadOutputFreezeRatio float64
}

func (c Config) withDefaults() Config {
	if c.EntryCode == "" {
		c.EntryCode = EntryCode
	}
	c.Limits = c.Limits.Normalize()
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 3
	}
	if c.AutoRetryMax < 0 {
		c.AutoRetryMax = 0
	}
	if c.RedoMaxPerSegment <= 0 {
		c.RedoMaxPerSegment = 3
	}
	if c.RedoMaxPerJob <= 0 {
		c.RedoMaxPerJob = 10
	}
	if c.ContinueWindow <= 0 {
		c.ContinueWindow = 24 * time.Hour
	}
	if c.DecisionReminderLead <= 0 {
		c.DecisionReminderLead = 4 * time.Hour
	}
	if c.PlanTTL <= 0 {
		c.PlanTTL = 7 * 24 * time.Hour
	}
	if c.QuoteTTL <= 0 {
		c.QuoteTTL = 10 * time.Minute
	}
	if c.SegmentDeadline <= 0 {
		c.SegmentDeadline = 25 * time.Minute
	}
	if c.ReapAfter <= 0 {
		c.ReapAfter = 5 * time.Minute
	}
	if c.VisionModelGroup == "" {
		c.VisionModelGroup = "vlm.qwen-3.8-flash"
	}
	if c.WriterMaxAttempts <= 0 {
		c.WriterMaxAttempts = 2
	}
	if c.WriterMaxAttempts > 3 {
		c.WriterMaxAttempts = 3
	}
	if c.WriterDeadline <= 0 {
		c.WriterDeadline = 3 * time.Minute
	}
	if c.EtaRoundP50 <= 0 {
		c.EtaRoundP50 = 4 * time.Minute
	}
	if c.EtaRoundP90 < c.EtaRoundP50 {
		c.EtaRoundP90 = c.EtaRoundP50 * 3 / 2
	}
	if c.FrameCount <= 0 || c.FrameCount > 9 {
		c.FrameCount = 9
	}
	if c.HoldTTL <= 0 {
		c.HoldTTL = 200 * time.Minute
	}
	if floor := c.HoldTTL - billing.LeaseDuration; c.RenewAfter < floor {
		c.RenewAfter = floor + time.Minute
	}
	if c.ActiveRepoll <= 0 {
		c.ActiveRepoll = 3 * time.Second
	}
	if c.SettleTimeout <= 0 {
		c.SettleTimeout = 15 * time.Second
	}
	if c.URLTTL <= 0 {
		c.URLTTL = time.Hour
	}
	if c.URLTTL > 24*time.Hour {
		c.URLTTL = 24 * time.Hour
	}
	if !ValidKeyPrefix(c.KeyPrefix) {
		c.KeyPrefix = ""
	}
	if c.ModerationTextGroup == "" {
		c.ModerationTextGroup = "moderation.text.default"
	}
	if c.ModerationImageGroup == "" {
		c.ModerationImageGroup = "moderation.image.default"
	}
	if c.ModerationTextDeadline <= 0 {
		c.ModerationTextDeadline = 5 * time.Second
	}
	if c.ModerationImageDeadline <= 0 {
		c.ModerationImageDeadline = 12 * time.Second
	}
	if c.ModerationRejectTTL <= 0 {
		c.ModerationRejectTTL = 24 * time.Hour
	}
	if c.ModerationConfirmWait <= 0 {
		c.ModerationConfirmWait = 3 * time.Second
	}
	if c.ModerationPlanWait < 0 {
		c.ModerationPlanWait = 0
	}
	if c.ConfirmThresholdPoints < 0 {
		c.ConfirmThresholdPoints = 0
	}
	if c.PreviewModelGroup == "" {
		c.PreviewModelGroup = "img.gpt-image-2.5-sunburst"
	}
	if c.PreviewDailyQuota <= 0 {
		c.PreviewDailyQuota = 3
	}
	if c.PreviewWait <= 0 {
		c.PreviewWait = 60 * time.Second
	}
	if c.CopyDailyQuota <= 0 {
		c.CopyDailyQuota = 20
	}
	if c.CopyWait <= 0 {
		c.CopyWait = 60 * time.Second
	}
	if c.BadOutputBlackRatio <= 0 || c.BadOutputBlackRatio > 1 {
		c.BadOutputBlackRatio = 0.6
	}
	if c.BadOutputFreezeRatio <= 0 || c.BadOutputFreezeRatio > 1 {
		c.BadOutputFreezeRatio = 0.8
	}
	c.AnalysisRules = c.AnalysisRules.Normalize()
	return c
}

// ValidKeyPrefix 幂等键环境前缀是否合法（空串合法）：只允许 [A-Za-z0-9._-]、≤32 字符（不含「:」，键里「:」之后是步骤键）。
func ValidKeyPrefix(p string) bool {
	if len(p) > 32 {
		return false
	}
	for _, c := range p {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// DefaultConfig 默认运行参数（自动重试默认开 1 次，见 DESIGN §5.14）。
func DefaultConfig() Config { return Config{AutoRetryMax: 1}.withDefaults() }

// Ledger 分段预留（每段一张预留单，DESIGN §9.2）。生产实现是 BillingLedger（包 pkg/billing）。
// 全部方法按 (userID, requestID) 幂等：同一请求号重复调用不会重复冻结 / 扣除 / 退回。
type Ledger interface {
	// Quote 一段的报价点数（只读，不动账）。
	Quote(ctx context.Context, c billing.Charge) (int64, error)
	// Hold 冻结；held=false 表示这次不计费（off / shadow / 0 点）。失败返回用户口径的业务错误。
	Hold(ctx context.Context, c billing.Charge) (held bool, err error)
	// Confirm 全额确认扣除；预留单已失效（过期回收 / 已退回）返回 ErrChargeGone，调用方不得交付。
	Confirm(ctx context.Context, userID int64, requestID string) error
	// Refund 退回；refunded=false 表示没有可退的预留单（未计费）。已扣除的单退回返回 ErrChargeConfirmed。
	Refund(ctx context.Context, userID int64, requestID, reason string) (refunded bool, err error)
	// Renew 续期一张仍 HELD 的预留单（到期顺延）；不是 HELD 的不动、不报错。
	Renew(ctx context.Context, userID int64, requestID string) error
	// Status 预留单真实状态（HELD / CONFIRMED / PARTIALLY_REFUNDED / REFUNDED / EXPIRED / CANCELLED；没有单或计费未开启为 ""）。
	Status(ctx context.Context, userID int64, requestID string) (string, error)
}

var (
	// ErrChargeGone 预留单已不是 HELD（过期被回收 / 已退回），不能再确认扣除。
	ErrChargeGone = errors.New("replicate: 预留单已失效")
	// ErrChargeConfirmed 预留单已扣除，不能退回（状态不一致，需人工核对）。
	ErrChargeConfirmed = errors.New("replicate: 预留单已扣除，不能退回")
	// ErrHoldUnknown 冻结调用已成功返回，但随后读预留单状态失败：钱可能已冻结。调用方不得当成「预留失败、没扣点数」，
	// 要按请求号幂等退回（或留给回收），文案说「冻结的点数会自动退回」。
	ErrHoldUnknown = errors.New("replicate: 冻结结果未知")
	// ErrHoldMismatch 冻结按请求号命中了已有预留单，但它的入口 / 模型组 / 点数与本次冻结不一致（请求号被别处先占用）。
	// 不是本段的单：调用方不得当成已预留，也不得按请求号退回它（那是别人的单）。
	ErrHoldMismatch = errors.New("replicate: 请求号已被其他预留单占用")
)

// Signer 播放 / 下载地址现签（*mediasvc.Client 直接满足）：rid → 带签名的公网读地址，不入库。
type Signer interface {
	ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error)
}

// URLOwner 本站存储地址的归属校验：url 是不是本站存储签发、且（上传目录里的）属于 userID。
// 生产实现见 app/replicate/httpapi（由存储配置推导域名，不设第三方白名单）；未装配时 http(s) 输入一律拒收。
type URLOwner func(ctx context.Context, userID int64, url string) bool

// Gateway ai-gateway 调用（*aigateway.Client 直接满足）。
type Gateway interface {
	Submit(ctx context.Context, req aigateway.CallRequest, opt aigateway.SubmitOptions) (*aigateway.Call, error)
	Get(ctx context.Context, id string, wait time.Duration) (*aigateway.Call, error)
}

// Media 媒体服务（*mediasvc.Client 直接满足）。
type Media interface {
	IngestURL(ctx context.Context, srcURL, purpose, idem string) (mediasvc.Resource, error)
	Probe(ctx context.Context, rid string) (mediasvc.ProbeInfo, error)
	// InspectVideo 坏片检测（黑屏 / 定格占比、能否解码），只读。
	InspectVideo(ctx context.Context, rid string) (mediasvc.VideoInspect, error)
	Frames(ctx context.Context, rid string, count int, idem string) ([]mediasvc.Frame, error)
	Grid(ctx context.Context, rids []string, p mediasvc.GridParams, idem string) (mediasvc.Resource, error)
}

// Cutter 按区间切出一段原片（重编码保精度），作为该段的参考视频。
//
// 占位：媒体服务目前只有 /video/trim（只截前 N 秒），没有区间切段 /video/cut（DESIGN §5.10 新算子）。
// 未装配（nil）时多段视频（> 单段上限）不开放：方案阶段直接给出「长视频分段暂未开放」，不会产生预留。
type Cutter interface {
	Cut(ctx context.Context, sourceRid string, t0, t1 float64, idem string) (rid string, err error)
}

// JobBuilder 按新插入的 job id 生成 task_id、步骤与预留镜像（请求号、幂等键都带 job id）。
type JobBuilder func(jobID int64) (taskID string, steps []*model.Step, charges []*model.Charge)

// Store 业务表读写（生产实现 app/replicate/repo，bun）。所有状态迁移都是 CAS：返回 false 表示没抢到（状态已变），
// 调用方放弃本次推进、不覆盖。找不到返回 (nil, nil)。
type Store interface {
	// 分析
	InsertAnalysis(ctx context.Context, a *model.Analysis) (got *model.Analysis, created bool, err error)
	GetAnalysis(ctx context.Context, id int64) (*model.Analysis, error)
	GetAnalysisByNo(ctx context.Context, userID int64, no string) (*model.Analysis, error)
	// UpdateAnalysis CAS：WHERE id=? AND stage=? AND attempt=? AND status IN (0,1)。
	UpdateAnalysis(ctx context.Context, id int64, stage string, attempt int, set map[string]any) (bool, error)
	// RestartAnalysis 失败的分析重新开始：WHERE id=? AND status=3 AND attempt=?，attempt+1、回到 ingest。
	RestartAnalysis(ctx context.Context, id int64, attempt int) (bool, error)
	DueAnalyses(ctx context.Context, limit int) ([]int64, error)

	// 方案
	InsertPlan(ctx context.Context, p *model.Plan) (got *model.Plan, created bool, err error)
	GetPlanByNo(ctx context.Context, userID int64, no string) (*model.Plan, error)
	GetPlan(ctx context.Context, id int64) (*model.Plan, error)
	// ConfirmPlan READY → CONFIRMED 并写 job_id。
	ConfirmPlan(ctx context.Context, id, jobID int64) (bool, error)
	// ReleasePlan 确认失败（预留没成功）时放回方案：WHERE id=? AND status=CONFIRMED AND job_id=?；
	// 占用期间已有改标注出的子方案 → STALE（superseded_by = 最新子方案），否则 READY。
	ReleasePlan(ctx context.Context, id, jobID int64) (bool, error)
	// RefreshPlan 过期的 READY 方案被同输入重新生成时刷新有效期与报价快照：WHERE id=? AND status=READY。
	RefreshPlan(ctx context.Context, id int64, expiresAt time.Time, quote []byte) (bool, error)

	// 出片任务
	// CreateJob 同一事务插入 job（提交中），再按新 id 由 build 生成 task_id、步骤与预留镜像（待预留）一并插入；
	// (user_id, request_id) 已存在返回已有 job 与 created=false（幂等）。
	CreateJob(ctx context.Context, j *model.Job, build JobBuilder) (got *model.Job, created bool, err error)
	GetJob(ctx context.Context, id int64) (*model.Job, error)
	GetJobByRequest(ctx context.Context, userID int64, requestID string) (*model.Job, error)
	// TransitJob CAS：WHERE id=? AND status IN (from...)。
	TransitJob(ctx context.Context, id int64, from []int8, to int8, set map[string]any) (bool, error)
	// ClaimContinue 待继续 → 继续中：WHERE id=? AND status=7 AND continue_attempt=?，continue_attempt+1。
	ClaimContinue(ctx context.Context, id int64, attempt int) (bool, error)
	ListSteps(ctx context.Context, jobID int64) ([]*model.Step, error)
	ListCharges(ctx context.Context, jobID int64) ([]*model.Charge, error)
	// UpdateStep CAS：WHERE id=? AND status=?。
	UpdateStep(ctx context.Context, id int64, from string, set map[string]any) (bool, error)
	// UpdateCharge CAS：WHERE id=? AND status IN (from...)。
	UpdateCharge(ctx context.Context, id int64, from []int8, set map[string]any) (bool, error)
	// AddCharges 继续其余段：同一事务插入预留镜像并把对应步骤的 billing_request_id 指过去（stepReq: 步骤 id → 请求号）。
	AddCharges(ctx context.Context, charges []*model.Charge, stepReq map[int64]string) error
	// AddRetryStep 自动重试：同一事务把原步骤 RUNNING→FAILED（只按 id+status CAS，不带 owner：重启后实例标识会变）
	// 并插入重试步骤（PENDING，共用预留单）。
	AddRetryStep(ctx context.Context, failed *model.Step, failSet map[string]any, retry *model.Step) (bool, error)
	// AddRedo 用户重做：同一事务插入步骤（HOLDING）+ 预留镜像（待预留）。(job_id, client_key) 已存在返回已有镜像与 created=false。
	AddRedo(ctx context.Context, step *model.Step, charge *model.Charge) (existing *model.Charge, created bool, err error)
	// DueJobs 需要推进的 job（租约空闲或过期、next_run_at 已到）：在途（排队 / 执行中 / 提交中 / 继续中）；
	// 待继续且已过继续截止时间；任何状态下还有可能动账的预留镜像（0/1/6）；终态里成片未合成 / 待重试。按 next_run_at 升序。
	DueJobs(ctx context.Context, limit int) ([]int64, error)
	// RescheduleJob / RescheduleAnalysis 写 next_run_at = 库时钟 + delay（不动 updated_at）。
	RescheduleJob(ctx context.Context, id int64, delay time.Duration) error
	RescheduleAnalysis(ctx context.Context, id int64, delay time.Duration) error
	// TouchJob 请求路径逐段预留时的心跳：WHERE id=? AND status IN (...) 刷新 updated_at；false = job 已不在这些状态（被回收）。
	TouchJob(ctx context.Context, id int64, statuses []int8) (bool, error)
	// FinishJob 汇总到终态：TransitJob 外加「不存在段号 ≤ maxSeg 的未终态出片步骤」（HOLDING/PENDING/RUNNING/DELIVERING），
	// 与并发的用户重做互斥（重做先插入 HOLDING 步骤，终态迁移就不会成功）。
	FinishJob(ctx context.Context, id int64, from []int8, to int8, maxSeg int, set map[string]any) (bool, error)
	// ClaimRedo 原子占一次重做名额并把 job 拉回执行中：redo_count = redo_count + 1, status = 1, finished_at = NULL, next_run_at = NULL
	// WHERE id=? AND redo_count < max AND status IN (...)。同一条语句里改状态：重做的预留进行期间 job 已不是终态，终态补扫不会误退它的单。
	ClaimRedo(ctx context.Context, id int64, max int, statuses []int8) (bool, error)
	// CancelOrphanSteps 终态 job 上没开始、却挂着预留单的 PENDING 出片步骤（提交回收在取消步骤前崩溃留下的）置取消，
	// 条件里联表核 job 仍是终态（与 ClaimRedo 把 job 拉回执行中互斥）。返回取消了几条。
	CancelOrphanSteps(ctx context.Context, jobID int64) (int64, error)
	// ReleaseRedo 预留没成功时还回名额：redo_count = redo_count - 1 WHERE id=? AND redo_count > 0。
	ReleaseRedo(ctx context.Context, id int64) error
	// ListJobs 本人的 job（id 倒序）与总数。
	ListJobs(ctx context.Context, userID int64, offset, limit int) ([]*model.Job, int64, error)

	// 偏好
	GetPref(ctx context.Context, userID int64) (*model.UserPref, error)
	SetAutoContinue(ctx context.Context, userID int64, on bool) error
}
