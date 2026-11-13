package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
)

// adminStuckAfter 非终态批次超过这段时间没有新的步骤记录，链路标为卡住。
const adminStuckAfter = 10 * time.Minute

// adminDiagnostics 后台批次详情的排查视图（任务链路 / 工作流步骤 / 诊断 / 时间线）。
// 字段口径对齐 Java 任务详情（creation-task.ts 的 TaskTrace / WorkflowStep / Diagnosis / TimelineEvent），
// 前端可复用同一套渲染。整体以 JSON 文本放进 AdminBatchDetail.diagnostics。
type adminDiagnostics struct {
	Trace         adminTrace           `json:"trace"`
	WorkflowSteps []adminWorkflowStep  `json:"workflowSteps"`
	Diagnosis     adminDiagnosis       `json:"diagnosis"`
	Timeline      []adminTimelineEvent `json:"timeline"`
	// Media 全部图片引用（组图 / 送生图图序 / 成品 / 蒙版）及其现签预览地址；
	// 每张子任务的 traceTaskId 也在这里，前端据此拉统一链路 /spans。
	Media *adminMedia `json:"media,omitempty"`
}

type adminTrace struct {
	TaskID      string            `json:"taskId"`
	TraceID     string            `json:"traceId"`
	GeneratedAt string            `json:"generatedAt"`
	StuckAt     *adminStuckAt     `json:"stuckAt"`
	Stages      []adminTraceStage `json:"stages"`
	Terminal    bool              `json:"terminal"`
}

type adminStuckAt struct {
	Stage     string `json:"stage"`
	Source    string `json:"source"`
	Since     string `json:"since"`
	ElapsedMs int64  `json:"elapsedMs"`
	Reason    string `json:"reason"`
}

type adminTraceStage struct {
	Key        string  `json:"key"`
	Source     string  `json:"source"`
	Name       string  `json:"name"`
	Status     string  `json:"status"` // done / running / failed / pending / skipped
	StartedAt  *string `json:"startedAt"`
	EndedAt    *string `json:"endedAt"`
	DurationMs *int64  `json:"durationMs"`
	Check      *string `json:"check"`
	Detail     *string `json:"detail"`
}

type adminWorkflowStep struct {
	StepPath      string  `json:"stepPath"`
	TaskID        string  `json:"taskId"`
	TraceTaskID   string  `json:"traceTaskId"`
	Status        string  `json:"status"`
	Operation     string  `json:"operation"`
	ModelGroup    *string `json:"modelGroup"`
	GatewayCallID *string `json:"gatewayCallId"`
	Attempt       int32   `json:"attempt"`
	StartedAt     *string `json:"startedAt"`
	FinishedAt    *string `json:"finishedAt"`
	ErrorMsg      *string `json:"errorMsg"`
}

type adminDiagnosis struct {
	Task        *adminDiagTask    `json:"task"`
	FailingStep *adminFailingStep `json:"failingStep"`
	Steps       adminStepCounts   `json:"steps"`
	Billing     adminDiagBilling  `json:"billing"`
	Hints       []adminHint       `json:"hints"`
}

type adminDiagTask struct {
	TaskID      string `json:"taskId"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	StatusLabel string `json:"statusLabel"`
	ErrorMsg    string `json:"errorMsg"`
	CreateTime  string `json:"createTime"`
	UpdateTime  string `json:"updateTime"`
}

type adminFailingStep struct {
	StepPath       string  `json:"stepPath"`
	Status         string  `json:"status"`
	Operation      string  `json:"operation"`
	FailureMessage string  `json:"failureMessage"`
	GatewayCallID  *string `json:"gatewayCallId"`
	AttemptCount   int32   `json:"attemptCount"`
	StartedAt      *string `json:"startedAt"`
	CompletedAt    *string `json:"completedAt"`
}

type adminStepCounts struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"byStatus"`
}

type adminDiagBilling struct {
	BillingStatus     string `json:"billingStatus"`
	ReservationStatus string `json:"reservationStatus"`
	PricePoints       int64  `json:"pricePoints"`
	Settled           bool   `json:"settled"`
}

type adminHint struct {
	Text string `json:"text"`
}

type adminTimelineEvent struct {
	ID       int    `json:"id"`
	Ts       string `json:"ts"`
	StepPath string `json:"stepPath"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Source   string `json:"source"`
}

// encodeAdminDiagnostics 把排查视图序列化成 JSON 文本；失败返回空串（详情页照常显示其余字段）。
func encodeAdminDiagnostics(b *model.Batch, tasks []*model.Task, now time.Time) string {
	return encodeAdminDiagnosticsWithMedia(nil, nil, config.FreeWatermarkConf{}, nil, b, tasks, now)
}

// encodeAdminDiagnosticsWithMedia 同上，并把所有图片引用解析成短期预览地址放进 media。
// traceID 给出子任务的统一链路任务号；nil 表示不带。
func encodeAdminDiagnosticsWithMedia(ctx context.Context, signer processSigner, c config.FreeWatermarkConf, traceID func(*model.Task) string, b *model.Batch, tasks []*model.Task, now time.Time) string {
	d := buildAdminDiagnostics(b, tasks, now)
	if ctx != nil {
		m := buildAdminMedia(ctx, signer, c, b, tasks, traceID)
		d.Media = &m
		for i := range d.WorkflowSteps {
			if i < len(m.Tasks) {
				d.WorkflowSteps[i].TaskID = m.Tasks[i].TaskID
				d.WorkflowSteps[i].TraceTaskID = m.Tasks[i].TraceTaskID
			}
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(raw)
}

func buildAdminDiagnostics(b *model.Batch, tasks []*model.Task, now time.Time) adminDiagnostics {
	logs := parseStepLogs(b.StepLog)
	terminal := isBatchTerminal(b.Status)

	stages := buildTraceStages(b, tasks, logs)
	trace := adminTrace{
		TaskID:      strconv.FormatInt(b.ID, 10),
		TraceID:     b.RequestID,
		GeneratedAt: now.Format(time.RFC3339),
		Stages:      stages,
		Terminal:    terminal,
	}
	if !terminal {
		trace.StuckAt = detectStuck(b, stages, now)
	}

	steps := buildWorkflowSteps(b, tasks)
	timeline := buildTimeline(b, tasks, logs)

	return adminDiagnostics{
		Trace:         trace,
		WorkflowSteps: steps,
		Diagnosis:     buildDiagnosis(b, tasks, logs, steps, trace.StuckAt),
		Timeline:      timeline,
	}
}

// ---------------------------------------------------------------- 任务链路

func buildTraceStages(b *model.Batch, tasks []*model.Task, logs []model.StepLog) []adminTraceStage {
	var stages []adminTraceStage

	// 提交：step_log 里有 submitted 即完成；提交失败留下 submit_aborted / hold_failed。
	if e, ok := lastLog(logs, "submit_aborted", "hold_failed"); ok {
		stages = append(stages, stageOf("batch.submit", "studio", "提交", "failed", e.At, e.At, nil, e.Detail))
	} else if e, ok := firstLog(logs, "submitted"); ok {
		stages = append(stages, stageOf("batch.submit", "studio", "提交", "done", e.At, e.At, nil, ""))
	} else {
		stages = append(stages, stageOf("batch.submit", "studio", "提交", "pending", 0, 0, nil, ""))
	}

	// 切图（仅商拍）：crop_plan / crop_check。
	stages = append(stages, logStage(logs, "crop.plan", "切图方案", pipeline.StepCropPlan))
	stages = append(stages, logStage(logs, "crop.check", "切图质检", pipeline.StepCropCheck))

	// 反推 / 姿态源与提示词：analyze、pose_source、pose_prompt 任一。
	stages = append(stages, logStage(logs, "prompt.analyze", "反推提示词",
		pipeline.StepAnalyze, pipeline.StepSource, pipeline.StepPrompt))

	// 生图提交网关：子任务是否已拿到 gateway_call_id。
	var withCall, total int
	var firstAt, lastAt int64
	for _, t := range tasks {
		total++
		if t.GatewayCallID.Valid && t.GatewayCallID.String != "" {
			withCall++
		}
		firstAt, lastAt = spanMs(firstAt, lastAt, t.CreatedAt.UnixMilli())
	}
	submitDetail := fmt.Sprintf("%d/%d 张已提交网关", withCall, total)
	switch {
	case total == 0:
		stages = append(stages, stageOf("gateway.call.created", "gateway", "提交生图网关", "pending", 0, 0, nil, ""))
	case withCall == total:
		stages = append(stages, stageOf("gateway.call.created", "gateway", "提交生图网关", "done", firstAt, lastAt, nil, submitDetail))
	case withCall > 0:
		stages = append(stages, stageOf("gateway.call.created", "gateway", "提交生图网关", "running", firstAt, 0, nil, submitDetail))
	default:
		stages = append(stages, stageOf("gateway.call.created", "gateway", "提交生图网关", "pending", firstAt, 0, nil, submitDetail))
	}

	// 生图交付与转存：成功 / 交付中 / 失败 / 仍在排队或生成中的子任务。
	var ok, delivering, failed, inflight int
	for _, t := range tasks {
		switch t.Status {
		case model.TaskSucceeded:
			ok++
		case model.TaskDelivering:
			delivering++
		case model.TaskFailed:
			failed++
		case model.TaskQueued, model.TaskRunning:
			inflight++
		}
	}
	deliverDetail := fmt.Sprintf("成功 %d · 交付中 %d · 生成中 %d · 失败 %d", ok, delivering, inflight, failed)
	switch {
	case total > 0 && ok+failed == total && failed == 0:
		stages = append(stages, stageOf("generate.deliver", "media", "生成与转存", "done", firstAt, lastAt, nil, deliverDetail))
	case total > 0 && ok == 0 && delivering == 0 && inflight == 0 && failed == total:
		stages = append(stages, stageOf("generate.deliver", "media", "生成与转存", "failed", firstAt, lastAt, nil, deliverDetail))
	case inflight > 0 || delivering > 0:
		stages = append(stages, stageOf("generate.deliver", "media", "生成与转存", "running", firstAt, 0, nil, deliverDetail))
	default:
		stages = append(stages, stageOf("generate.deliver", "media", "生成与转存", "pending", 0, 0, nil, deliverDetail))
	}

	// 结算：冻结中的点数是否已结清。
	var held, settled int
	for _, t := range tasks {
		switch t.BillingStatus {
		case model.BillingHeld:
			held++
		case model.BillingSettled, model.BillingRefunded, model.BillingShadow:
			settled++
		}
	}
	settleDetail := fmt.Sprintf("已结清 %d · 冻结中 %d", settled, held)
	switch {
	case held > 0 && isBatchTerminal(b.Status):
		check := "billing.held_after_terminal"
		stages = append(stages, stageOf("task.settle", "studio", "结算", "failed", 0, 0, &check, settleDetail))
	case held == 0 && settled > 0:
		stages = append(stages, stageOf("task.settle", "studio", "结算", "done", 0, 0, nil, settleDetail))
	case settled > 0:
		stages = append(stages, stageOf("task.settle", "studio", "结算", "running", 0, 0, nil, settleDetail))
	default:
		// 点数在提交时就冻结，未结清本身不代表结算在进行，等交付后再结算。
		stages = append(stages, stageOf("task.settle", "studio", "结算", "pending", 0, 0, nil, settleDetail))
	}

	return stages
}

// stageOf 组装一个阶段；start/end 为 unix 毫秒，0 表示无；end 为 0 且有 start 时视作进行中。
func stageOf(key, source, name, status string, start, end int64, check *string, detail string) adminTraceStage {
	st := adminTraceStage{Key: key, Source: source, Name: name, Status: status, Check: check}
	if start > 0 {
		s := msToRFC3339(start)
		st.StartedAt = &s
	}
	if end > 0 {
		e := msToRFC3339(end)
		st.EndedAt = &e
	}
	if start > 0 && end > start {
		d := end - start
		st.DurationMs = &d
	}
	if detail != "" {
		d := detail
		st.Detail = &d
	}
	return st
}

// logStage 由 step_log 中同名步骤推导阶段：有记录即完成，带 detail 且非降级视为失败。
func logStage(logs []model.StepLog, key, name string, steps ...string) adminTraceStage {
	first, okFirst := firstLog(logs, steps...)
	last, okLast := lastLog(logs, steps...)
	if !okFirst {
		return stageOf(key, "studio", name, "pending", 0, 0, nil, "")
	}
	status := "done"
	detail := ""
	if okLast && last.Detail != "" && !last.Degraded {
		status = "failed"
		detail = last.Detail
	} else if last.Degraded {
		detail = "已降级（读图失败，已改用文字信息）"
	}
	var check *string
	if status == "failed" {
		c := key + ".failed"
		check = &c
	}
	return stageOf(key, "studio", name, status, first.At, last.At, check, detail)
}

// detectStuck 找到最后一个运行中的阶段；它的开始时间（或批次最近更新时间）距今超过阈值即视为卡住。
func detectStuck(b *model.Batch, stages []adminTraceStage, now time.Time) *adminStuckAt {
	var running *adminTraceStage
	for i := range stages {
		if stages[i].Status == "running" {
			running = &stages[i]
		}
	}
	if running == nil {
		return nil
	}
	since := b.UpdatedAt
	if running.StartedAt != nil {
		if t, err := time.Parse(time.RFC3339, *running.StartedAt); err == nil {
			since = t
		}
	}
	elapsed := now.Sub(since)
	if elapsed < adminStuckAfter {
		return nil
	}
	return &adminStuckAt{
		Stage:     running.Key,
		Source:    running.Source,
		Since:     since.Format(time.RFC3339),
		ElapsedMs: elapsed.Milliseconds(),
		Reason:    fmt.Sprintf("「%s」已 %d 分钟没有进展", running.Name, int(elapsed.Minutes())),
	}
}

// ---------------------------------------------------------------- 工作流步骤（每张子任务一步）

func buildWorkflowSteps(b *model.Batch, tasks []*model.Task) []adminWorkflowStep {
	out := make([]adminWorkflowStep, 0, len(tasks))
	for _, t := range tasks {
		step := adminWorkflowStep{
			StepPath:  taskStepPath(t),
			Status:    taskStatusKey(t.Status),
			Operation: "generate",
			Attempt:   t.Attempt,
		}
		if t.ModelGroup != "" {
			mg := t.ModelGroup
			step.ModelGroup = &mg
		}
		if t.GatewayCallID.Valid && t.GatewayCallID.String != "" {
			gc := t.GatewayCallID.String
			step.GatewayCallID = &gc
		}
		if t.StartedAt.Valid {
			s := t.StartedAt.Time.Format(time.RFC3339)
			step.StartedAt = &s
		}
		if t.FinishedAt.Valid {
			f := t.FinishedAt.Time.Format(time.RFC3339)
			step.FinishedAt = &f
		}
		if t.ErrorReason.Valid && t.ErrorReason.String != "" {
			e := t.ErrorReason.String
			step.ErrorMsg = &e
		}
		out = append(out, step)
	}
	return out
}

func taskStepPath(t *model.Task) string {
	return fmt.Sprintf("generate/m%d-s%d-p%d", t.ModelIndex, t.ShotIndex, t.PoseIndex)
}

// ---------------------------------------------------------------- 诊断

func buildDiagnosis(b *model.Batch, tasks []*model.Task, logs []model.StepLog, steps []adminWorkflowStep, stuck *adminStuckAt) adminDiagnosis {
	batchStatus := batchStatusName(b.Status)
	d := adminDiagnosis{
		Task: &adminDiagTask{
			TaskID:      strconv.FormatInt(b.ID, 10),
			Type:        string(b.Kind),
			Status:      batchStatus,
			StatusLabel: batchStatus,
			CreateTime:  b.CreatedAt.Format(time.RFC3339),
			UpdateTime:  b.UpdatedAt.Format(time.RFC3339),
		},
		Steps: adminStepCounts{Total: len(steps), ByStatus: map[string]int{}},
	}
	if b.ErrorReason.Valid {
		d.Task.ErrorMsg = b.ErrorReason.String
	}

	for _, s := range steps {
		d.Steps.ByStatus[s.Status]++
	}

	// 失败点：第一张失败的子任务；整批在生图前失败时取最后一条失败的 step_log。
	for _, t := range tasks {
		if t.Status != model.TaskFailed {
			continue
		}
		f := &adminFailingStep{
			StepPath:       taskStepPath(t),
			Status:         taskStatusKey(t.Status),
			Operation:      "generate",
			FailureMessage: t.ErrorReason.String,
			AttemptCount:   t.Attempt,
		}
		if t.GatewayCallID.Valid && t.GatewayCallID.String != "" {
			gc := t.GatewayCallID.String
			f.GatewayCallID = &gc
		}
		if t.StartedAt.Valid {
			s := t.StartedAt.Time.Format(time.RFC3339)
			f.StartedAt = &s
		}
		if t.FinishedAt.Valid {
			c := t.FinishedAt.Time.Format(time.RFC3339)
			f.CompletedAt = &c
		}
		d.FailingStep = f
		break
	}
	if d.FailingStep == nil && b.Status == model.BatchFailed {
		if e, ok := lastLog(logs, "failed", "submit_aborted", "hold_failed"); ok {
			d.FailingStep = &adminFailingStep{
				StepPath:       "batch/" + e.Step,
				Status:         batchStatus,
				Operation:      e.Step,
				FailureMessage: e.Detail,
				CompletedAt:    ptrRFC3339(e.At),
			}
		}
	}

	d.Billing = buildDiagBilling(b, tasks)
	d.Hints = buildHints(b, tasks, d.FailingStep, stuck)
	return d
}

func buildDiagBilling(b *model.Batch, tasks []*model.Task) adminDiagBilling {
	var held, settled, refunded int
	for _, t := range tasks {
		switch t.BillingStatus {
		case model.BillingHeld:
			held++
		case model.BillingSettled, model.BillingShadow:
			settled++
		case model.BillingRefunded:
			refunded++
		}
	}
	out := adminDiagBilling{PricePoints: b.EstimatedPoints, Settled: held == 0 && settled > 0}
	switch {
	case held > 0:
		out.BillingStatus = "HELD"
		out.ReservationStatus = "held"
	case settled > 0:
		out.BillingStatus = "SETTLED"
		out.ReservationStatus = "settled"
	case refunded > 0:
		out.BillingStatus = "REFUNDED"
		out.ReservationStatus = "refunded"
	default:
		out.BillingStatus = "NONE"
		out.ReservationStatus = "none"
	}
	return out
}

func buildHints(b *model.Batch, tasks []*model.Task, failing *adminFailingStep, stuck *adminStuckAt) []adminHint {
	var hints []adminHint
	if stuck != nil {
		hints = append(hints, adminHint{Text: stuck.Reason + "，请检查对应依赖（反推 sidecar / ai-gateway / 媒体服务）"})
	}
	if failing != nil && failing.FailureMessage != "" {
		hints = append(hints, adminHint{Text: "失败原因：" + failing.FailureMessage + "；可在子任务上重做或强制失败"})
	}
	var held int
	for _, t := range tasks {
		if t.BillingStatus == model.BillingHeld {
			held++
		}
	}
	if held > 0 && isBatchTerminal(b.Status) {
		hints = append(hints, adminHint{Text: fmt.Sprintf("批次已结束但仍有 %d 张子任务的点数处于冻结状态，检查结算与对账", held)})
	}
	return hints
}

// ---------------------------------------------------------------- 时间线

func buildTimeline(b *model.Batch, tasks []*model.Task, logs []model.StepLog) []adminTimelineEvent {
	var events []adminTimelineEvent
	for _, e := range logs {
		status := "ok"
		msg := e.Detail
		switch {
		case e.Degraded:
			status = "degraded"
		case e.Detail != "":
			status = "failed"
		}
		events = append(events, adminTimelineEvent{
			Ts:       msToRFC3339(e.At),
			StepPath: e.Step,
			Kind:     "step",
			Status:   status,
			Message:  msg,
			Source:   "studio",
		})
	}
	for _, t := range tasks {
		path := taskStepPath(t)
		events = append(events, adminTimelineEvent{
			Ts: t.CreatedAt.Format(time.RFC3339), StepPath: path, Kind: "task", Status: "created", Source: "studio",
		})
		if t.StartedAt.Valid {
			events = append(events, adminTimelineEvent{
				Ts: t.StartedAt.Time.Format(time.RFC3339), StepPath: path, Kind: "task", Status: "started", Source: "studio",
			})
		}
		if t.FinishedAt.Valid {
			events = append(events, adminTimelineEvent{
				Ts: t.FinishedAt.Time.Format(time.RFC3339), StepPath: path, Kind: "task",
				Status: taskStatusKey(t.Status), Message: t.ErrorReason.String, Source: "studio",
			})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts < events[j].Ts })
	for i := range events {
		events[i].ID = i + 1
	}
	return events
}

// ---------------------------------------------------------------- 小工具

func firstLog(logs []model.StepLog, names ...string) (model.StepLog, bool) {
	for _, e := range logs {
		if containsStr(names, e.Step) {
			return e, true
		}
	}
	return model.StepLog{}, false
}

func lastLog(logs []model.StepLog, names ...string) (model.StepLog, bool) {
	for i := len(logs) - 1; i >= 0; i-- {
		if containsStr(names, logs[i].Step) {
			return logs[i], true
		}
	}
	return model.StepLog{}, false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func spanMs(lo, hi, v int64) (int64, int64) {
	if v <= 0 {
		return lo, hi
	}
	if lo == 0 || v < lo {
		lo = v
	}
	if v > hi {
		hi = v
	}
	return lo, hi
}

func msToRFC3339(ms int64) string {
	return time.UnixMilli(ms).Format(time.RFC3339)
}

func ptrRFC3339(ms int64) *string {
	if ms <= 0 {
		return nil
	}
	s := msToRFC3339(ms)
	return &s
}

func isBatchTerminal(s int8) bool {
	switch s {
	case model.BatchDone, model.BatchFailed, model.BatchPartial, model.BatchCanceled:
		return true
	}
	return false
}

func taskStatusKey(s int8) string {
	switch s {
	case model.TaskQueued:
		return "QUEUED"
	case model.TaskRunning:
		return "RUNNING"
	case model.TaskSucceeded:
		return "SUCCEEDED"
	case model.TaskFailed:
		return "FAILED"
	case model.TaskCanceled:
		return "CANCELED"
	case model.TaskDelivering:
		return "DELIVERING"
	}
	return "UNKNOWN"
}
