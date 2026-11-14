package repo

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/lease"

	"github.com/uptrace/bun"
)

// TaskRepo afv_studio_task。
type TaskRepo struct{ db bun.IDB }

// BulkInsert 一条多行 INSERT 插入同批次的子任务。
// 回填的 ID 来自 LastInsertId 顺推，依赖自增连续；要用主键（生成 task_id）的场景请插入后 ListByBatch 读回，别信回填值。
func (r *TaskRepo) BulkInsert(ctx context.Context, tasks []*model.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	_, err := r.db.NewInsert().Model(&tasks).Exec(ctx)
	return err
}

// BackfillTaskIDs 回填对外任务号与计费请求号：一条 UPDATE … CASE id WHEN … END，只填 task_id 仍为空的行。
// taskIDs: 主键 → task_id；billingPrefix 非空时同时写 billing_request_id = billingPrefix + task_id（如 "studio-"）。
func (r *TaskRepo) BackfillTaskIDs(ctx context.Context, taskIDs map[int64]string, billingPrefix string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(taskIDs))
	for id := range taskIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var tid, bid strings.Builder
	args := make([]any, 0, len(ids)*2)
	bargs := make([]any, 0, len(ids)*2)
	tid.WriteString("CASE id")
	bid.WriteString("CASE id")
	for _, id := range ids {
		tid.WriteString(" WHEN ? THEN ?")
		bid.WriteString(" WHEN ? THEN ?")
		args = append(args, id, taskIDs[id])
		bargs = append(bargs, id, billingPrefix+taskIDs[id])
	}
	tid.WriteString(" END")
	bid.WriteString(" END")
	q := r.db.NewUpdate().Table(TableTask).Set("task_id = "+tid.String(), args...)
	if billingPrefix != "" {
		q = q.Set("billing_request_id = "+bid.String(), bargs...)
	}
	n, err := affected(q.Where("id IN (?)", bun.In(ids)).Where("task_id IS NULL").Exec(ctx))
	if err != nil {
		return err
	}
	if int(n) != len(ids) {
		return errors.New("repo: 回填任务号的行数与预期不符")
	}
	return nil
}

// GetByID 按主键取（含已删除）。
func (r *TaskRepo) GetByID(ctx context.Context, id int64) (*model.Task, error) {
	t := new(model.Task)
	return noRows(t, r.db.NewSelect().Model(t).Where("t.id = ?", id).Limit(1).Scan(ctx))
}

// GetByTaskID 按对外任务号取（uk_task_id，含已删除；用户侧自行校验 user_id 与 deleted）。
func (r *TaskRepo) GetByTaskID(ctx context.Context, taskID string) (*model.Task, error) {
	t := new(model.Task)
	return noRows(t, r.db.NewSelect().Model(t).Where("t.task_id = ?", taskID).Limit(1).Scan(ctx))
}

// ListByBatch 批次的全部子任务，按拆分顺序（model_index, shot_index, pose_index, id）。
func (r *TaskRepo) ListByBatch(ctx context.Context, batchID int64) ([]*model.Task, error) {
	var list []*model.Task
	err := r.db.NewSelect().Model(&list).Where("t.batch_id = ?", batchID).
		OrderExpr("t.model_index ASC, t.shot_index ASC, t.pose_index ASC, t.id ASC").Scan(ctx)
	return list, err
}

// ListByBatches 多个批次的子任务（历史列表一次取齐），按 batch_id 分组返回。
func (r *TaskRepo) ListByBatches(ctx context.Context, batchIDs []int64) (map[int64][]*model.Task, error) {
	out := map[int64][]*model.Task{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	var list []*model.Task
	err := r.db.NewSelect().Model(&list).Where("t.batch_id IN (?)", bun.In(batchIDs)).
		OrderExpr("t.batch_id ASC, t.model_index ASC, t.shot_index ASC, t.pose_index ASC, t.id ASC").Scan(ctx)
	for _, t := range list {
		out[t.BatchID] = append(out[t.BatchID], t)
	}
	return out, err
}

// ListByStatus 某状态的子任务（轮询器用），最多 limit 条。只取未封禁（admin_locked=0）
// 且没有租约或租约已过期的（别的实例正在轮询的不列）。
//
// 顺序按「上次轮询时间」轮转：从没轮询过的（lease_until 为 NULL）在前，其余按 lease_until 升序——
// 每次轮询结束 MarkPolled 把 lease_until 记成结束时刻（见 KeepLease），刚轮询过的排到队尾。
// 这样在途超过 limit 时每个子任务都能轮到，不会总是同一批（按 updated_at 排时轮询不改 updated_at，前 limit 条永远不变）。
func (r *TaskRepo) ListByStatus(ctx context.Context, status int8, limit int) ([]*model.Task, error) {
	var list []*model.Task
	err := r.db.NewSelect().Model(&list).Where("t.status = ?", status).Where("t.admin_locked = 0").Where(lease.FreeCond("t")).
		OrderExpr("t.lease_until IS NOT NULL ASC, t.lease_until ASC, t.updated_at ASC, t.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// MarkPolled 记下「刚轮询过」：释放租约后把 lease_until 写成 NOW(3)（已过期，不挡别人抢），供 ListByStatus 轮转排序。
// 只改仍无人持有的行（期间被别的实例抢走则不动）；显式 updated_at = updated_at，不算业务变化。
func (r *TaskRepo) MarkPolled(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE `"+TableTask+"` SET `lease_until` = NOW(3), `updated_at` = `updated_at` "+
		"WHERE `id` = ? AND `lease_owner` IS NULL AND `lease_until` IS NULL", id)
	return err
}

// ListRenewable 需要续期的子任务：所在批次未终态（排队 / 切图中 / 反推中 / 生图中），任务在途（排队 / 生成中 / 交付中），
// 有计费请求号（不看 billing_status 镜像：镜像写失败停在 0 的也要续，不是 HELD 的预留单 Renew 是空操作）。
// 按 id 升序取 id > afterID 的至多 limit 条（只取续期要用的列），调用方据此游标轮转。
func (r *TaskRepo) ListRenewable(ctx context.Context, afterID int64, limit int) ([]*model.Task, error) {
	var list []*model.Task
	err := r.db.NewSelect().Model(&list).Column("t.id", "t.batch_id", "t.user_id", "t.billing_request_id", "t.billing_status").
		Join("JOIN afv_studio_batch AS b ON b.id = t.batch_id").
		Where("b.status IN (?)", bun.In([]int8{model.BatchQueued, model.BatchCropping, model.BatchPrompting, model.BatchGenerating})).
		Where("t.status IN (?)", bun.In([]int8{model.TaskQueued, model.TaskRunning, model.TaskDelivering})).
		Where("t.billing_request_id IS NOT NULL AND t.billing_request_id <> ''").
		Where("t.id > ?", afterID).
		OrderExpr("t.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ClaimRetry 后台重做先占轮次：attempt 仍是 attempt、任务仍是失败 / 已取消且未封禁时 attempt+1。返回是否占到。
// 占到的轮次只属于这一次重做（attempt 只增不减），据此生成的计费请求号不会与并发 / 撤销过的重做重复。
func (r *TaskRepo) ClaimRetry(ctx context.Context, id int64, attempt int32) (bool, error) {
	return r.ClaimRetryFrom(ctx, id, attempt, []int8{model.TaskFailed, model.TaskCanceled})
}

// ClaimRetryFrom 同 ClaimRetry，可指定允许的状态（编辑提示词重新生成也收 成功）。
func (r *TaskRepo) ClaimRetryFrom(ctx context.Context, id int64, attempt int32, statuses []int8) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableTask).Set("attempt = attempt + 1").
		Where("id = ?", id).Where("attempt = ?", attempt).
		Where("status IN (?)", bun.In(statuses)).
		Where("admin_locked = 0").Exec(ctx))
	return n == 1, err
}

// ReleaseRetry 退还 ClaimRetry 占到但没用上的轮次（attempt claimed → claimed-1）：仅当 attempt 仍是 claimed、
// 任务仍是失败 / 已取消、未封禁、billing_request_id 仍是占轮次前的原号（oldReq，空串匹配 NULL / 空）时才退。
// ClaimRetry 只动 attempt，所以这几个条件都成立 = 占轮次之后没有任何别的写入。返回是否退到。
func (r *TaskRepo) ReleaseRetry(ctx context.Context, id int64, claimed int32, oldReq string) (bool, error) {
	return r.ReleaseRetryFrom(ctx, id, claimed, oldReq, []int8{model.TaskFailed, model.TaskCanceled})
}

// ReleaseRetryFrom 同 ReleaseRetry，状态集合与 ClaimRetryFrom 一致。
func (r *TaskRepo) ReleaseRetryFrom(ctx context.Context, id int64, claimed int32, oldReq string, statuses []int8) (bool, error) {
	q := r.db.NewUpdate().Table(TableTask).Set("attempt = attempt - 1").
		Where("id = ?", id).Where("attempt = ?", claimed).
		Where("status IN (?)", bun.In(statuses)).
		Where("admin_locked = 0")
	if oldReq == "" {
		q = q.Where("(billing_request_id IS NULL OR billing_request_id = '')")
	} else {
		q = q.Where("billing_request_id = ?", oldReq)
	}
	n, err := affected(q.Exec(ctx))
	return n == 1, err
}

// ListFinalHeld 任务已终态（成功 / 失败 / 已取消）而计费镜像仍是 已冻结 的子任务（计费善后用）：
// 例如结果未知的任务置失败后预留单留给回收器，到期释放时没人回写镜像。按 id 升序，最多 limit 条。
func (r *TaskRepo) ListFinalHeld(ctx context.Context, limit int) ([]*model.Task, error) {
	var list []*model.Task
	err := r.db.NewSelect().Model(&list).Column("t.id", "t.user_id", "t.billing_request_id", "t.billing_status").
		Where("t.status IN (?)", bun.In([]int8{model.TaskSucceeded, model.TaskFailed, model.TaskCanceled})).
		Where("t.billing_status = ?", model.BillingHeld).
		Where("t.billing_request_id IS NOT NULL AND t.billing_request_id <> ''").
		OrderExpr("t.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// CASBillingStatus 仅当镜像仍是 from 时改成 to（与提交 / 结算 / 退款的写入互斥，谁先谁赢）。返回是否改到。
func (r *TaskRepo) CASBillingStatus(ctx context.Context, id int64, from, to int8) (bool, error) {
	res, err := r.db.NewUpdate().Table(TableTask).Set("billing_status = ?", to).
		Where("id = ?", id).Where("billing_status = ?", from).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BatchesWithStrayQueued 已终态却还有排队子任务的批次号（后台重做后批次拉回失败的兜底）。
// 从排队的子任务出发（idx_status），量很小。
func (r *TaskRepo) BatchesWithStrayQueued(ctx context.Context, limit int) ([]int64, error) {
	var ids []int64
	err := r.db.NewSelect().Model((*model.Task)(nil)).ColumnExpr("DISTINCT t.batch_id").
		Join("JOIN afv_studio_batch AS b ON b.id = t.batch_id").
		Where("t.status = ?", model.TaskQueued).Where("t.admin_locked = 0").
		Where("b.status IN (?)", bun.In([]int8{model.BatchDone, model.BatchFailed, model.BatchPartial, model.BatchCanceled})).
		OrderExpr("t.batch_id ASC").Limit(int64(limit)).Scan(ctx, &ids)
	return ids, err
}

// CountByStatus 某状态的子任务数（如生成中的并发占用）。
func (r *TaskRepo) CountByStatus(ctx context.Context, status int8) (int64, error) {
	return r.db.NewSelect().Model((*model.Task)(nil)).Where("t.status = ?", status).Count(ctx)
}

// TransitStatus CAS 迁移子任务状态。进 生成中 时补 started_at=NOW(3)，进终态时补 finished_at=NOW(3)（extra 里已给的不覆盖）。
func (r *TaskRepo) TransitStatus(ctx context.Context, id int64, from, to int8, extra map[string]any) (bool, error) {
	set := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		set[k] = v
	}
	if _, ok := set["started_at"]; !ok && to == model.TaskRunning {
		set["started_at"] = bun.Safe("NOW(3)")
	}
	if _, ok := set["finished_at"]; !ok && model.TaskFinal(to) {
		set["finished_at"] = bun.Safe("NOW(3)")
	}
	if r, ok := set["error_reason"].(string); ok {
		set["error_reason"] = Trunc(r, 255)
	}
	return bundb.TransitStatus(ctx, r.db, TableTask, id, int(from), int(to), set)
}

// SetPrompt 写生图提示词、反推元信息、实际送生图的图序（JSON 可为空，空则不改）。
func (r *TaskRepo) SetPrompt(ctx context.Context, id int64, text string, meta, inputImages json.RawMessage) error {
	q := r.db.NewUpdate().Table(TableTask).Set("prompt_text = ?", text)
	if len(meta) > 0 {
		q = q.Set("prompt_meta = CAST(? AS JSON)", string(meta))
	}
	if len(inputImages) > 0 {
		q = q.Set("input_images = CAST(? AS JSON)", string(inputImages))
	}
	_, err := q.Where("id = ?", id).Exec(ctx)
	return err
}

// SetGateway 写 ai-gateway 调用号与状态原文。callID 为空只更新状态。
// 写调用号时顺带记 started_at（真正提交到 gateway 的时刻，生图时限从这里算）：换了新调用号记 NOW(3)，
// 同调用号（幂等重提）保留原值。started_at 必须排在 gateway_call_id 之前赋值（MySQL 按书写顺序求值，要读旧调用号）。
func (r *TaskRepo) SetGateway(ctx context.Context, id int64, callID, status string) error {
	q := r.db.NewUpdate().Table(TableTask).Set("gateway_status = ?", Trunc(status, 32))
	if callID != "" {
		q = q.Set("started_at = IF(gateway_call_id <=> ?, COALESCE(started_at, NOW(3)), NOW(3))", callID).
			Set("gateway_call_id = ?", callID)
	}
	_, err := q.Where("id = ?", id).Exec(ctx)
	return err
}

// SetResult 写成品：资源号、公有桶地址、宽高、上游成本（DECIMAL 文本，空=不改）。
func (r *TaskRepo) SetResult(ctx context.Context, id int64, rid, url string, w, h int32, costCNY string) error {
	q := r.db.NewUpdate().Table(TableTask).
		Set("result_rid = ?", rid).Set("result_url = ?", url).
		Set("result_width = ?", w).Set("result_height = ?", h)
	if costCNY != "" {
		q = q.Set("cost_cny = ?", costCNY)
	}
	_, err := q.Where("id = ?", id).Exec(ctx)
	return err
}

// SetBilling 写计费请求号与计费状态（model.Billing*）。requestID 为空只改状态。
func (r *TaskRepo) SetBilling(ctx context.Context, id int64, requestID string, status int8) error {
	q := r.db.NewUpdate().Table(TableTask).Set("billing_status = ?", status)
	if requestID != "" {
		q = q.Set("billing_request_id = ?", requestID)
	}
	_, err := q.Where("id = ?", id).Exec(ctx)
	return err
}

// SetBillingStatusByBatch 整批改计费状态（提交时全部冻结成功后一次写 已冻结 / 影子）。
func (r *TaskRepo) SetBillingStatusByBatch(ctx context.Context, batchID int64, status int8) error {
	_, err := r.db.NewUpdate().Table(TableTask).Set("billing_status = ?", status).Where("batch_id = ?", batchID).Exec(ctx)
	return err
}

// SetError 写失败原因（截到 255 字）。
func (r *TaskRepo) SetError(ctx context.Context, id int64, reason string) error {
	_, err := r.db.NewUpdate().Table(TableTask).Set("error_reason = ?", Trunc(reason, 255)).Where("id = ?", id).Exec(ctx)
	return err
}

// CancelQueued 把批次里仍在排队的子任务一次性置为 已取消，返回条数。
// 注意：不退款，调用方需先 ListByBatch 拿到这些任务的 billing_request_id 再逐个退（或改用逐个 TransitStatus）。
func (r *TaskRepo) CancelQueued(ctx context.Context, batchID int64, reason string) (int64, error) {
	return affected(r.db.NewUpdate().Table(TableTask).
		Set("status = ?", model.TaskCanceled).
		Set("finished_at = NOW(3)").
		Set("error_reason = ?", Trunc(reason, 255)).
		Where("batch_id = ?", batchID).Where("status = ?", model.TaskQueued).Exec(ctx))
}

// FailQueued 把批次里仍在排队的子任务置为 失败（提交冻结失败 / 切图反推整批失败），返回条数。不退款。
func (r *TaskRepo) FailQueued(ctx context.Context, batchID int64, reason string) (int64, error) {
	return affected(r.db.NewUpdate().Table(TableTask).
		Set("status = ?", model.TaskFailed).
		Set("finished_at = NOW(3)").
		Set("error_reason = ?", Trunc(reason, 255)).
		Where("batch_id = ?", batchID).Where("status = ?", model.TaskQueued).Exec(ctx))
}

// CountByBatch 批次内各状态的子任务数（status → 数量）。
func (r *TaskRepo) CountByBatch(ctx context.Context, batchID int64) (map[int8]int, error) {
	var rows []struct {
		Status int8 `bun:"status"`
		N      int  `bun:"n"`
	}
	err := r.db.NewSelect().Model((*model.Task)(nil)).ColumnExpr("t.status AS status, COUNT(*) AS n").
		Where("t.batch_id = ?", batchID).GroupExpr("t.status").Scan(ctx, &rows)
	out := map[int8]int{}
	for _, x := range rows {
		out[x.Status] = x.N
	}
	return out, err
}

// SoftDeleteByBatch 随批次软删子任务（任务中心靠同步收敛删除态）。
func (r *TaskRepo) SoftDeleteByBatch(ctx context.Context, batchID int64) error {
	_, err := r.db.NewUpdate().Table(TableTask).Set("deleted = 1").Where("batch_id = ?", batchID).Where("deleted = 0").Exec(ctx)
	return err
}

// BatchStat 批次的子任务汇总（后台列表用）。
type BatchStat struct {
	CostCNY    string // 上游成本合计（DECIMAL 文本）
	ModelGroup string // 本批使用的 model group（同批一致，取 MIN）
}

// StatsByBatches 多个批次的成本合计与 model group。
// TODO(sqlc)：设计 §9.5 定成本汇总走 pkg/auditdb；该包就位前先在这里用 SUM（只读，口径一致）。
func (r *TaskRepo) StatsByBatches(ctx context.Context, batchIDs []int64) (map[int64]BatchStat, error) {
	out := map[int64]BatchStat{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		BatchID    int64  `bun:"batch_id"`
		Cost       string `bun:"cost"`
		ModelGroup string `bun:"model_group"`
	}
	err := r.db.NewSelect().Model((*model.Task)(nil)).
		ColumnExpr("t.batch_id AS batch_id, CAST(SUM(t.cost_cny) AS CHAR) AS cost, MIN(t.model_group) AS model_group").
		Where("t.batch_id IN (?)", bun.In(batchIDs)).GroupExpr("t.batch_id").Scan(ctx, &rows)
	for _, x := range rows {
		out[x.BatchID] = BatchStat{CostCNY: x.Cost, ModelGroup: x.ModelGroup}
	}
	return out, err
}

// ListSettledSucceeded 付费产物入库对账（logic/asset_reconcile.go）：成功且已确认扣费（billing_status=已结算）的子任务，
// 附加键集分页条件 cond（pkg/asset.KeysetCond 生成，列为 t.updated_at / t.id），按 updated_at, id 升序（走 idx_sync_updated_id）。
// 不看 deleted：用户删除批次不影响付费成品入素材库（与转存 outbox 同一口径）。
func (r *TaskRepo) ListSettledSucceeded(ctx context.Context, cond string, args []any, limit int) ([]*model.Task, error) {
	var list []*model.Task
	err := r.db.NewSelect().Model(&list).Where("t.status = ?", model.TaskSucceeded).Where("t.billing_status = ?", model.BillingSettled).
		Where(cond, args...).OrderExpr("t.updated_at ASC, t.id ASC").Limit(int64(max(1, limit))).Scan(ctx)
	return list, err
}

// AcquireLease 抢子任务的行租约（不心跳；对账补入这类一次性短操作用，用完 ReleaseLease）。
func (r *TaskRepo) AcquireLease(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	return lease.Acquire(ctx, r.db, TableTask, id, owner, ttl)
}

// ReleaseLease 释放子任务的行租约（只对 owner 生效）。
func (r *TaskRepo) ReleaseLease(ctx context.Context, id int64, owner string) error {
	return lease.Release(ctx, r.db, TableTask, id, owner)
}
