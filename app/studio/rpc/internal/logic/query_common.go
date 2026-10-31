package logic

import (
	"context"
	"database/sql"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// PromptSummaryLen 用户侧提示词摘要长度（字符）。完整 prompt 只在后台给。
const PromptSummaryLen = 200

// stageProgress 批次阶段对应的进度：提交中 / 排队 0 / 切图中 10 / 反推中 30 / 生图中 60 / 终态 100。
func stageProgress(batchStatus int8) int32 {
	switch batchStatus {
	case model.BatchQueued, model.BatchSubmitting:
		return 0
	case model.BatchCropping:
		return 10
	case model.BatchPrompting:
		return 30
	case model.BatchGenerating:
		return 60
	}
	return 100
}

// TaskProgress 子任务进度（估算，不是厂商真实进度）：终态 100；交付中 95；生成中至少 60；排队跟批次阶段。
func TaskProgress(batchStatus, taskStatus int8) int32 {
	if model.TaskFinal(taskStatus) {
		return 100
	}
	if taskStatus == model.TaskDelivering {
		return 95
	}
	p := stageProgress(batchStatus)
	if p == 100 { // 批次已终态但子任务还没收敛（取消中），按生成中算
		p = 60
	}
	if taskStatus == model.TaskRunning && p < 60 {
		p = 60
	}
	return p
}

// PromptSummary 提示词摘要：去掉首行图序声明后取前 200 字。
// 约定（与编排侧一致）：有送生图的输入图（input_images 非空）时 prompt_text 首行是图序声明，正文从第二行起；
// 没有输入图（纯文生图）时整段都是正文。
func PromptSummary(t *model.Task) string {
	s := t.PromptText.String
	if len(t.InputImages) > 0 && string(t.InputImages) != "null" && string(t.InputImages) != "[]" {
		if _, body, ok := strings.Cut(s, "\n"); ok {
			s = body
		}
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > PromptSummaryLen {
		return string(r[:PromptSummaryLen])
	}
	return s
}

func msOf(t sql.NullTime) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.UnixMilli()
}

func toTaskInfo(t *model.Task, batchStatus int8) *studio.TaskInfo {
	return &studio.TaskInfo{
		Id: t.ID, TaskId: t.TaskID.String, BatchId: t.BatchID, Kind: string(t.Kind),
		ModelIndex: t.ModelIndex, ShotIndex: t.ShotIndex, PoseIndex: t.PoseIndex,
		Status: int32(t.Status), Progress: TaskProgress(batchStatus, t.Status),
		ResultUrl: t.ResultURL.String, ResultWidth: t.ResultWidth, ResultHeight: t.ResultHeight,
		ErrorReason: t.ErrorReason.String, PromptSummary: PromptSummary(t), ModelGroup: t.ModelGroup,
		CreatedAt: t.CreatedAt.UnixMilli(), StartedAt: msOf(t.StartedAt), FinishedAt: msOf(t.FinishedAt),
	}
}

func toBatchInfo(b *model.Batch, tasks []*model.Task) *studio.BatchInfo {
	info := &studio.BatchInfo{
		Id: b.ID, Kind: string(b.Kind), Status: int32(b.Status), RequestId: b.RequestID, Payload: string(b.Payload),
		TaskTotal: b.TaskTotal, TaskDone: b.TaskDone, TaskFailed: b.TaskFailed, EstimatedPoints: b.EstimatedPoints,
		ErrorReason: b.ErrorReason.String, CreatedAt: b.CreatedAt.UnixMilli(), UpdatedAt: b.UpdatedAt.UnixMilli(),
	}
	for _, t := range tasks {
		if t.Status == model.TaskSucceeded && t.ResultURL.String != "" {
			info.CoverUrl = t.ResultURL.String
			break
		}
	}
	return info
}

func toBatchDetail(b *model.Batch, tasks []*model.Task) *studio.BatchDetail {
	out := &studio.BatchDetail{Batch: toBatchInfo(b, tasks), Tasks: make([]*studio.TaskInfo, 0, len(tasks))}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, toTaskInfo(t, b.Status))
	}
	return out
}

// ownedBatch 用户侧取批次：不存在 / 不是本人 / 已删除一律 NotFound。
func (d *deps) ownedBatch(ctx context.Context, batchID, userID int64) (*model.Batch, error) {
	b, err := d.repo.Batch.GetByID(ctx, batchID, userID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, xerr.NotFoundf("批次")
	}
	return b, nil
}

// cancel 取消批次：只取消排队中的子任务（CAS → 已取消，逐个退款）；生成中的不动（由轮询器收敛），
// 返回本次取消数与仍在生成中的数量。批次：没有进行中的子任务时按结果重算终态（全取消 → 已取消）。
func (d *deps) cancel(ctx context.Context, batchID, userID int64) (*model.Batch, int, int, error) {
	log := logx.WithContext(ctx)
	b, err := d.ownedBatch(ctx, batchID, userID)
	if err != nil {
		return nil, 0, 0, err
	}
	if model.BatchFinal(b.Status) {
		return nil, 0, 0, xerr.New(xerr.Conflict, "批次已结束，不能取消")
	}
	if b.Status == model.BatchSubmitting {
		return nil, 0, 0, xerr.New(xerr.Conflict, "正在提交，请稍后再试")
	}
	tasks, err := d.repo.Task.ListByBatch(ctx, b.ID)
	if err != nil {
		return nil, 0, 0, err
	}
	canceled, running := 0, 0
	for _, t := range tasks {
		switch t.Status {
		case model.TaskRunning, model.TaskDelivering:
			running++
			continue
		case model.TaskQueued:
		default:
			continue
		}
		ok, err := d.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, model.TaskCanceled, map[string]any{"error_reason": "已取消"})
		if err != nil {
			return nil, 0, 0, err
		}
		if !ok { // 轮询器刚把它提交出去了
			running++
			continue
		}
		canceled++
		if refundable(d.bill.Mode(), t) {
			rctx, cancel := detached(ctx)
			d.bill.Fail(rctx, b.UserID, t.BillingRequestID.String, "用户取消，点数已退回")
			cancel()
			if err := d.repo.Task.SetBilling(ctx, t.ID, "", model.BillingRefunded); err != nil {
				log.Errorf("studio: 子任务 %d 写退款状态失败: %v", t.ID, err)
			}
		}
	}
	if err := d.repo.Batch.UpdateCounters(ctx, b.ID, 0, canceled); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
	if running == 0 {
		counts, err := d.repo.Task.CountByBatch(ctx, b.ID)
		if err != nil {
			return nil, 0, 0, err
		}
		if counts[model.TaskQueued] == 0 && counts[model.TaskRunning] == 0 && counts[model.TaskDelivering] == 0 {
			to := model.BatchCanceled
			if counts[model.TaskSucceeded] > 0 {
				to = model.BatchPartial
			}
			if ok, err := d.repo.Batch.UpdateStatus(ctx, b.ID, b.Status, to, nil); err != nil {
				return nil, 0, 0, err
			} else if !ok {
				log.Infof("studio: 批次 %d 取消时状态已变（%d），交给轮询器收敛", b.ID, b.Status)
			}
		}
	}
	if b, err = d.repo.Batch.GetByID(ctx, b.ID, 0); err != nil {
		return nil, 0, 0, err
	}
	return b, canceled, running, nil
}

// ---------------- 后台 ----------------

func jsonText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

func toAdminBatch(b *model.Batch, tasks []*model.Task, st repo.BatchStat) *studio.AdminBatch {
	cost := st.CostCNY
	if cost == "" {
		cost = "0"
	}
	return &studio.AdminBatch{
		Batch: toBatchInfo(b, tasks), UserId: b.UserID, WorkspaceId: b.WorkspaceID,
		CostCny: cost, ModelGroup: st.ModelGroup,
	}
}

func toAdminTask(t *model.Task, batchStatus int8) *studio.AdminTask {
	var dur int64
	if t.StartedAt.Valid && t.FinishedAt.Valid {
		dur = t.FinishedAt.Time.Sub(t.StartedAt.Time).Milliseconds()
	}
	return &studio.AdminTask{
		Task: toTaskInfo(t, batchStatus), PromptText: t.PromptText.String,
		PromptMeta: jsonText(t.PromptMeta), InputImages: jsonText(t.InputImages),
		GatewayCallId: t.GatewayCallID.String, GatewayStatus: t.GatewayStatus.String,
		Attempt: t.Attempt, AdminLocked: t.AdminLocked != 0, CostCny: t.CostCNY,
		BillingRequestId: t.BillingRequestID.String, BillingStatus: int32(t.BillingStatus), DurationMs: dur,
	}
}
