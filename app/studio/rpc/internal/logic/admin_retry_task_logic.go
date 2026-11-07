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

// adminRetry 只收未删除批次里未封禁（admin_locked=0）的失败 / 已取消子任务：
//  1. 先 CAS 占轮次（ClaimRetry：attempt 仍是读到的值才 +1）。attempt 只增不减，占到的轮次只属于这一次重做，
//     据此生成的计费请求号 studio-<task_id>-r<attempt> 不会与并发的另一次重做、或撤销过的重做重复
//     （旧做法先算号再冻结：并发两次会冻结同一单，CAS 失败方把对方在用的单退掉；撤销回退 attempt 后再重做会拿回已退回的单）。
//  2. 用新号冻结（Hold），并核对预留单确实是 HELD（freeze 遇已有幂等键原样返回旧单，已退回 / 过期的不能当成功）。
//  3. CAS 失败/已取消 → 排队：清错误与 gateway 字段、换计费号，交 Poller 重新提交（幂等键追加 :r<attempt>，不会拿回旧的终态记录）。
//
// 批次已终态时拉回：prompt 已就绪 → 生图中；还没反推过（编排阶段整批失败）→ 排队，重新编排（有缓存）。
// 任何一步失败要退新单时，先重读任务：只有库里的 billing_request_id ≠ 新号（新单确实没被用上）才退，避免退掉在用的单。
func (f *flow) adminRetry(ctx context.Context, t *model.Task, b *model.Batch) error {
	return f.retryTask(ctx, t, b, stepAdminRetry)
}

// retryTask 重做的公共流程（运营重做 / 用户重试共用，见 adminRetry 注释）；step 是落 step_log 的步骤名。
// 用户重试的额外前置条件在 retry_task_logic.go 里先判，这里只做占轮次 → 冻结新单 → 排队 → 批次拉回。
func (f *flow) retryTask(ctx context.Context, t *model.Task, b *model.Batch, step string) error {
	log := logx.WithContext(ctx)
	if b.Deleted != 0 {
		return xerr.New(xerr.Conflict, "批次已删除，不能重试")
	}
	if t.AdminLocked != 0 {
		return xerr.New(xerr.Conflict, "已强制失败，不能重试")
	}
	if t.Status != model.TaskFailed && t.Status != model.TaskCanceled {
		return xerr.New(xerr.Conflict, "只有失败或已取消的任务可以重试")
	}
	if b.Status == model.BatchSubmitting {
		return xerr.New(xerr.Conflict, "批次正在提交，请稍后再试")
	}
	charge, err := f.retryCharge(t, b)
	if err != nil {
		return err
	}
	const stale = "任务状态已变化，请刷新后再看"
	claimed, err := f.repo.Task.ClaimRetry(ctx, t.ID, t.Attempt)
	if err != nil {
		return err
	}
	if !claimed { // 另一次重做先占了轮次 / 状态已变
		return xerr.New(xerr.Conflict, stale)
	}
	attempt := t.Attempt + 1
	reqID := retryBillingRequestID(t.TaskID.String, int(attempt))
	charge.RequestID = reqID
	hctx, cancel := detached(ctx)
	defer cancel()
	if _, err := f.bill.Hold(hctx, charge); err != nil {
		// 冻结没成功：确认计费服务里没有这张新单后退还轮次，否则（结果未知）轮次作废、下次换下一个号
		f.releaseUnheldRetry(hctx, t, b.UserID, attempt, reqID)
		return err
	}
	const undoReason = "未能开始重做，点数已退回"
	if f.bill.Mode() == billing.ModeOn {
		st, err := f.bill.Status(hctx, b.UserID, reqID)
		if err != nil {
			log.Errorf("studio: 重做子任务 %d 查新预留单 %s 状态失败，退回: %v", t.ID, reqID, err)
			f.bill.Fail(hctx, b.UserID, reqID, undoReason) // 号是本次独占的，没人在用
			return xerr.New(xerr.ServerError, "暂时无法计费，请稍后重试")
		}
		if st != "" && st != resvHeld { // 拿回的是已结束的旧单（不该发生：轮次独占），不能当冻结成功
			log.Errorf("studio: 重做子任务 %d 的预留单 %s 不是冻结态（%s），拒绝重做", t.ID, reqID, st)
			return xerr.New(xerr.Conflict, "计费单状态异常，请稍后重试")
		}
	}
	extra := map[string]any{
		"error_reason": nil, "gateway_call_id": nil, "gateway_status": nil,
		"billing_request_id": reqID, "billing_status": billingStatusOf(f.bill.Mode()),
		"started_at": nil, "finished_at": nil,
	}
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, t.Status, model.TaskQueued, extra)
	if err != nil || !ok {
		f.failUnusedRetry(hctx, t.ID, b.UserID, reqID, undoReason)
		if err != nil {
			return err
		}
		return xerr.New(xerr.Conflict, stale)
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
	} else {
		f.reopenIfConverged(ctx, b.ID)
	}
	f.stepLog(ctx, b.ID, model.StepLog{Step: step, Detail: t.TaskID.String})
	return nil
}

// reopenIfConverged 读到批次时它还在生图中（同批另一次重试在跑），但在本任务回到排队之前，那次重试可能已结束、
// 批次已按当时的计数收敛成终态（收敛只在没有在途子任务时发生）。重读一次：已是 全部成功 / 全部失败 / 部分成功 就拉回生图中，
// 让本任务照常派发、之后由 converge 按实际计数重算终态与 task_done / task_failed。CAS 失败（已被拉回）不算错；
// 已取消的不拉回；重读失败只记日志，交给 Poller 的终态批次排队子任务兜底（rescueStrayQueued）。
func (f *flow) reopenIfConverged(ctx context.Context, batchID int64) {
	log := logx.WithContext(ctx)
	cur, err := f.repo.Batch.GetByID(ctx, batchID, 0)
	if err != nil || cur == nil {
		log.Errorf("studio: 批次 %d 重读失败（交给 Poller 兜底）: %v", batchID, err)
		return
	}
	switch cur.Status {
	case model.BatchDone, model.BatchFailed, model.BatchPartial:
	default:
		return
	}
	if ok, err := f.repo.Batch.UpdateStatus(ctx, batchID, cur.Status, model.BatchGenerating, map[string]any{"error_reason": nil}); err != nil || !ok {
		log.Errorf("studio: 批次 %d 已收敛，拉回生图中未成功（交给 Poller 兜底）: ok=%v err=%v", batchID, ok, err)
	}
}

// retryCharge 重做冻结 1 张的计费参数（不含请求号）：与提交同一入口码 / 档位 / 分辨率口径。
// 用户重试的余额预检（Enough）与 retryTask 的冻结共用它，保证两边算的是同一个价。
func (f *flow) retryCharge(t *model.Task, b *model.Batch) (billing.Charge, error) {
	p, err := parsePayload(b.Payload)
	if err != nil {
		return billing.Charge{}, err
	}
	res := p.Resolution
	if r, _, err := pipeline.NormalizeForGroup(t.ModelGroup, p.Resolution, p.AspectRatio); err == nil {
		res = r // 与提交时同口径（历史批次的快照可能是未归一的档位）
	}
	entry := svc.EntryCode(f.cfg, t.Kind)
	return billing.Charge{
		UserID: b.UserID, EntryCode: entry, Kind: billing.KindQuick, Count: 1,
		Tier: tierOf(entry, res), Resolution: strings.ToUpper(res),
	}, nil
}

// releaseUnheldRetry 冻结失败后退还刚占到的轮次（attempt → attempt-1），让失败的冻结（如点数不足）不白白消耗重试次数。
// 只在能确认新号 reqID 在计费服务里没有预留单时才退：退还后下次重做会再用同一个号，若那张单其实存在（冻结超时但已落库），
// 同号冻结会原样拿回它——仍是 HELD 就照常用上，已了结的会被 retryTask 的 HELD 核对拒绝，因此查不清（查询出错 / 单已存在）就不退，
// 轮次作废、下次换下一个号。计费非 on（off / shadow）时 Hold 不会建单，可直接退。
// 退还是 CAS（见 repo.ReleaseRetry）：占轮次之后任务已被别处改过就不退。尽力而为，只记日志。
func (f *flow) releaseUnheldRetry(ctx context.Context, t *model.Task, userID int64, attempt int32, reqID string) {
	log := logx.WithContext(ctx)
	if f.bill.Mode() == billing.ModeOn {
		st, err := f.bill.Status(ctx, userID, reqID)
		if err != nil || st != "" {
			log.Errorf("studio: 子任务 %d 冻结 %s 失败，预留单状态不明（%q, %v），第 %d 轮作废不退还", t.ID, reqID, st, err, attempt)
			return
		}
	}
	ok, err := f.repo.Task.ReleaseRetry(ctx, t.ID, attempt, t.BillingRequestID.String)
	if err != nil || !ok {
		log.Errorf("studio: 子任务 %d 退还第 %d 轮失败（任务已变化或库不可用）: ok=%v err=%v", t.ID, attempt, ok, err)
		return
	}
	log.Infow("studio retry attempt released after hold failure", logx.Field("task_id", t.TaskID.String), logx.Field("attempt", attempt))
}

// failUnusedRetry 退回一次重做冻结的新单，但只在它确实没被用上时：重读任务，库里的 billing_request_id ≠ reqID 才退。
// 重读失败（库不可用）不退：宁可留给回收器到期释放，也不退掉可能在用的单。
func (f *flow) failUnusedRetry(ctx context.Context, taskID, userID int64, reqID, reason string) {
	cur, err := f.repo.Task.GetByID(ctx, taskID)
	if err != nil || cur == nil {
		logx.WithContext(ctx).Errorf("studio: 子任务 %d 重读失败，新预留单 %s 留给回收器: %v", taskID, reqID, err)
		return
	}
	if cur.BillingRequestID.String == reqID {
		return // 新单已写进任务（被用上了），不能退
	}
	f.bill.Fail(ctx, userID, reqID, reason)
}

// undoRetry 撤销一次重做：子任务 排队→原状态（原字段写回，attempt 不回退：轮次已用掉，下次重做换新号），
// 计数还原，新预留单在确实没被用上时退回。都是尽力而为。
func (f *flow) undoRetry(ctx context.Context, t *model.Task, b *model.Batch, reqID, reason string) {
	log := logx.WithContext(ctx)
	back := map[string]any{
		"error_reason": nullStr(t.ErrorReason), "gateway_call_id": nullStr(t.GatewayCallID),
		"gateway_status": nullStr(t.GatewayStatus), "billing_request_id": nullStr(t.BillingRequestID),
		"billing_status": t.BillingStatus, "started_at": nullTime(t.StartedAt), "finished_at": nullTime(t.FinishedAt),
	}
	ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, t.Status, back)
	if err != nil || !ok {
		// 没撤回（已被派发 / 库不可用）：新预留单留给它用，Poller 兜底会拉回批次
		log.Errorf("studio: 子任务 %d 撤销重做失败（交给 Poller 兜底）: ok=%v err=%v", t.ID, ok, err)
		if err != nil {
			f.failUnusedRetry(ctx, t.ID, b.UserID, reqID, reason) // 写入结果不明：按库里实际情况决定退不退
		}
		return
	}
	f.failUnusedRetry(ctx, t.ID, b.UserID, reqID, reason)
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
