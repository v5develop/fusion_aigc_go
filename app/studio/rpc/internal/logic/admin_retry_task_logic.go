package logic

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

type AdminRetryTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminRetryTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminRetryTaskLogic {
	return &AdminRetryTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminRetryTask 运营重做失败 / 已取消的子任务（flow.adminRetry）。
func (l *AdminRetryTaskLogic) AdminRetryTask(in *studio.AdminTaskReq) (*studio.Empty, error) {
	if in.Id <= 0 && strings.TrimSpace(in.TaskId) == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务")
	}
	t, b, err := adminTask(l.ctx, l.svcCtx.Repo, in)
	if err != nil {
		return nil, err
	}
	if err := newAdminFlow(l.svcCtx).adminRetry(l.ctx, t, b); err != nil {
		return nil, err
	}
	l.Infow("studio admin retry", logx.Field("task_id", t.TaskID.String), logx.Field("attempt", t.Attempt+1), logx.Field("operator", in.OperatorId))
	return &studio.Empty{}, nil
}

// adminRetry 只收未封禁（admin_locked=0）的失败 / 已取消子任务：
// attempt+1、换新计费请求号重新冻结（studio-<task_id>-r<attempt>，旧单已退回不能再确认）、清错误与 gateway 字段，
// CAS → 排队，交 Poller 重新提交（幂等键追加 :r<attempt>，不会拿回旧的终态记录）。
// 批次已终态时拉回：prompt 已就绪 → 生图中；还没反推过（编排阶段整批失败）→ 排队，重新编排（有缓存）。
// 拉回出错：子任务 CAS 回原状态（原字段原样写回）、新预留单退回、报错；Poller 另有兜底（rescueStrayQueued）。
func (f *flow) adminRetry(ctx context.Context, t *model.Task, b *model.Batch) error {
	log := logx.WithContext(ctx)
	if t.AdminLocked != 0 {
		return xerr.New(xerr.Conflict, "已强制失败，不能重试")
	}
	if t.Status != model.TaskFailed && t.Status != model.TaskCanceled {
		return xerr.New(xerr.Conflict, "只有失败或已取消的任务可以重试")
	}
	if b.Status == model.BatchSubmitting {
		return xerr.New(xerr.Conflict, "批次正在提交，请稍后再试")
	}
	p, err := parsePayload(b.Payload)
	if err != nil {
		return err
	}
	res := p.Resolution
	if r, _, err := pipeline.NormalizeForGroup(t.ModelGroup, p.Resolution, p.AspectRatio); err == nil {
		res = r // 与提交时同口径（历史批次的快照可能是未归一的档位）
	}
	attempt := t.Attempt + 1
	reqID := retryBillingRequestID(t.TaskID.String, int(attempt))
	entry := svc.EntryCode(f.cfg, t.Kind)
	hctx, cancel := detached(ctx)
	defer cancel()
	if _, err := f.bill.Hold(hctx, billing.Charge{
		UserID: b.UserID, RequestID: reqID, EntryCode: entry, Kind: billing.KindQuick, Count: 1,
		Tier: tierOf(entry, res), Resolution: strings.ToUpper(res),
	}); err != nil {
		return err
	}
	const undoReason = "未能开始重做，点数已退回"
	extra := map[string]any{
		"attempt": attempt, "error_reason": nil, "gateway_call_id": nil, "gateway_status": nil,
		"billing_request_id": reqID, "billing_status": billingStatusOf(f.bill.Mode()),
		"started_at": nil, "finished_at": nil,
	}
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, t.Status, model.TaskQueued, extra)
	if err != nil || !ok {
		f.bill.Fail(hctx, b.UserID, reqID, undoReason)
		if err != nil {
			return err
		}
		return xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
	}
	if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 0, -1); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
	if model.BatchFinal(b.Status) {
		to := model.BatchGenerating
		if !t.PromptText.Valid || t.PromptText.String == "" {
			to = model.BatchQueued
		}
		// ok=false：批次已被别处拉回（如 Poller 兜底），不算失败
		if _, err := f.repo.Batch.UpdateStatus(ctx, b.ID, b.Status, to, map[string]any{"error_reason": nil}); err != nil {
			log.Errorf("studio: 批次 %d 拉回进行中失败，撤销重做: %v", b.ID, err)
			f.undoRetry(hctx, t, b, reqID, undoReason)
			return err
		}
	}
	f.stepLog(ctx, b.ID, model.StepLog{Step: stepAdminRetry, Detail: t.TaskID.String})
	return nil
}

// undoRetry 撤销一次重做：子任务 排队→原状态（原字段写回），计数还原，新预留单退回。都是尽力而为。
func (f *flow) undoRetry(ctx context.Context, t *model.Task, b *model.Batch, reqID, reason string) {
	log := logx.WithContext(ctx)
	back := map[string]any{
		"attempt": t.Attempt, "error_reason": nullStr(t.ErrorReason), "gateway_call_id": nullStr(t.GatewayCallID),
		"gateway_status": nullStr(t.GatewayStatus), "billing_request_id": nullStr(t.BillingRequestID),
		"billing_status": t.BillingStatus, "started_at": nullTime(t.StartedAt), "finished_at": nullTime(t.FinishedAt),
	}
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, t.Status, back)
	if err != nil || !ok {
		// 没撤回（已被派发 / 库不可用）：新预留单留给它用，Poller 兜底会拉回批次
		log.Errorf("studio: 子任务 %d 撤销重做失败（交给 Poller 兜底）: ok=%v err=%v", t.ID, ok, err)
		return
	}
	f.bill.Fail(ctx, b.UserID, reqID, reason)
	if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 0, 1); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
}

func nullStr(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// nullTime 时间列写回：用 FROM_UNIXTIME 交给库按会话时区换算（不直接传 time.Time，见 model 包注释）。
func nullTime(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return bun.Safe(fmt.Sprintf("FROM_UNIXTIME(%d / 1000)", v.Time.UnixMilli()))
}
