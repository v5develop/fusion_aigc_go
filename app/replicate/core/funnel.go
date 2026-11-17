package core

import (
	"context"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// ---------------------------------------------------------------------------------------------
// 业务漏斗埋点（CONTRACT §L1 / AGENT §13.8）：上传 → 方案卡 → 确认转化 → 第 1 段后继续率 → 保存 → 重做率 → 坏片 → 取消。
//
//   - 每一步打一次计数 replicate_funnel_total{stage, channel}（Prometheus 抓取，看实时），并落一行事件到 afv_replicate_funnel
//     （按天出漏斗 / 转化率，SQL 见 deploy/grafana/replicate-funnel.sql）。
//   - channel = agent（智能体内部入口）/ web（REST）/ system（轮询器等没有入口的系统推进：第 1 段做完、成片交付、坏片…）。
//     系统事件的 channel 取同一个 job / 分析上最早一条用户事件的渠道（查不到才记 system），所以按渠道切分的转化率口径一致。
//   - 尽力而为：写库失败只打告警（判据=funnel_event_write_failed），绝不影响业务调用，也不碰任何资金表。
//   - 去重：dedupe_key 唯一，重放 / 多实例并发推进只记一次；计数只在「这一行确实是新写入的」时才加，两边口径一致。
// ---------------------------------------------------------------------------------------------

// 漏斗阶段（replicate_funnel_total 的 stage 标签，也是 afv_replicate_funnel.stage）。
const (
	StageAnalysisCreate  = "analysis_create"  // 发起拆解（新建分析；复用已有分析不算）
	StageAnalysisSucceed = "analysis_succeed" // 拆解成功（带 multi_person）
	StagePlanCreate      = "plan_create"      // 方案出卡（普通方案 / 示例）
	StagePlanEdit        = "plan_edit"        // 改标注 / 再做一条产生的新方案
	StagePreview         = "preview"          // 定妆图生成（回放不算）
	StageConfirmFirst    = "confirm_first"    // 确认：先做第 1 段
	StageConfirmAll      = "confirm_all"      // 确认：全部（单段任务也记在这里）
	StageBatchConfirm    = "batch_confirm"    // 批量确认（一批一条，plan_id = 源方案；批内每条任务另有一条 confirm_all）
	StageFirstDone       = "first_done"       // 第 1 段做完、进入「待继续」决策
	StageContinue        = "continue"         // 继续其余段（用户点或自动继续）
	StageDecisionTimeout = "decision_timeout" // 决策超时自动结束（只留第 1 段）
	StageDelivered       = "delivered"        // 成片交付（整条成片合成成功，首次）
	StageSegmentSave     = "segment_save"     // 手动保存单段到素材库
	StageRedo            = "redo"             // 单段重做（失败段）
	StageRefine          = "refine"           // 已成功段带备注精修
	StageBadOutput       = "bad_output"       // 坏片判定
	StageStop            = "stop"             // 用户在决策卡上选择不继续
	StageCancelRest      = "cancel_rest"      // 用户取消剩余段
	StagePublishCopy     = "publish_copy"     // 生成发布文案（回放不算；一成片一平台一次）
	StageExportRatio     = "export_ratio"     // 多比例导出（回放 / 同比例返回成片本身不算；一成片一比例一模式一次）

	// StageAnalysisSucceedMulti 仅计数用的派生阶段：拆解成功且原片多人（主体 ≥ 2），与 analysis_succeed 同时加一，
	// 看板据此算「多人原片占比」；不单独落库（库里用 analysis_succeed.multi_person）。
	StageAnalysisSucceedMulti = "analysis_succeed_multi"
)

// 渠道。
const (
	ChannelWeb    = "web"
	ChannelAgent  = "agent"
	ChannelSystem = "system"
)

// 事件写库的单次时限（脱离请求上下文，避免客户端断开让事件丢失；也防止库慢拖住业务）。
const funnelWriteTimeout = 2 * time.Second

var funnelTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "replicate",
	Subsystem: "funnel",
	Name:      "total",
	Help:      "视频做同款业务漏斗：按阶段与渠道（agent / web / system）计数",
	Labels:    []string{"stage", "channel"},
})

type channelKey struct{}

// WithChannel 把调用渠道放进 ctx：Ops 层按入口（REST / 智能体）设置，内核打点时读取。
func WithChannel(ctx context.Context, channel string) context.Context {
	if channel == "" {
		return ctx
	}
	return context.WithValue(ctx, channelKey{}, channel)
}

// ChannelOf ctx 里的渠道（没设置为空）。
func ChannelOf(ctx context.Context) string {
	c, _ := ctx.Value(channelKey{}).(string)
	return c
}

// FunnelStore 漏斗事件落库（生产实现 app/replicate/repo；可选）。
type FunnelStore interface {
	// InsertFunnelEvent INSERT IGNORE：dedupe_key 已存在返回 inserted=false。
	InsertFunnelEvent(ctx context.Context, e *model.FunnelEvent) (inserted bool, err error)
	// FunnelChannel 取同一分析 / job 上最早一条用户事件（agent / web）的渠道；没有返回空串。
	FunnelChannel(ctx context.Context, analysisID, jobID int64) (string, error)
}

// WithFunnel 装配漏斗事件落库；不装配只有计数。
func WithFunnel(f FunnelStore) Option { return func(s *Service) { s.funnelStore = f } }

// funnelEvent 一条漏斗事件的归属与去重键。
type funnelEvent struct {
	Stage      string
	UserID     int64
	AnalysisID int64
	PlanID     int64
	JobID      int64
	SegIndex   int
	Multi      bool   // 原片主体 ≥ 2（只在分析成功 / 方案事件上有意义）
	Dedupe     string // 唯一键，必填
}

// funnel 记一次漏斗事件：永不返回错误、永不 panic 出去。
func (s *Service) funnel(ctx context.Context, e funnelEvent) {
	defer func() {
		if r := recover(); r != nil {
			logx.WithContext(ctx).Errorf("replicate: 漏斗打点异常 判据=funnel_event_write_failed stage=%s: %v", e.Stage, r)
		}
	}()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), funnelWriteTimeout)
	defer cancel()
	channel := ChannelOf(ctx)
	if channel == "" && s.funnelStore != nil && (e.JobID > 0 || e.AnalysisID > 0) {
		if c, err := s.funnelStore.FunnelChannel(wctx, e.AnalysisID, e.JobID); err == nil {
			channel = c
		}
	}
	if channel == "" {
		channel = ChannelSystem
	}
	counted := true
	if s.funnelStore != nil {
		row := &model.FunnelEvent{Stage: e.Stage, Channel: channel, UserID: e.UserID, AnalysisID: e.AnalysisID, PlanID: e.PlanID,
			JobID: e.JobID, SegIndex: e.SegIndex, DedupeKey: e.Dedupe}
		if e.Multi {
			row.MultiPerson = 1
		}
		inserted, err := s.funnelStore.InsertFunnelEvent(wctx, row)
		if err != nil {
			logx.WithContext(ctx).Errorf("replicate: 漏斗事件写库失败 判据=funnel_event_write_failed stage=%s job=%d plan=%d analysis=%d: %v",
				e.Stage, e.JobID, e.PlanID, e.AnalysisID, err)
		} else {
			counted = inserted // 重放 / 并发重复：库里已有这一行，不重复计数
		}
	}
	if counted {
		funnelTotal.Inc(e.Stage, channel)
		if e.Multi && e.Stage == StageAnalysisSucceed {
			funnelTotal.Inc(StageAnalysisSucceedMulti, channel)
		}
	}
}

// funnelMulti 分析结果里的主体数是否 ≥ 2（多人原片）。
func funnelMulti(an *AnalysisResult) bool { return an != nil && len(an.Subjects) >= 2 }

// planFunnelStage 方案落库来源 → 阶段：改标注（patch:）/ 再做一条（reuse:）是派生方案，其余是新方案。
func planFunnelStage(o planOpts) string {
	if o.parentID != nil || strings.HasPrefix(o.salt, "patch:") || strings.HasPrefix(o.salt, "reuse:") {
		return StagePlanEdit
	}
	return StagePlanCreate
}
