package repo

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/lease"

	"github.com/uptrace/bun"
)

// RunRepo afv_canvas_node_run。
type RunRepo struct{ db bun.IDB }

// Insert 新建 run（不含 task_id / billing_request_id；正常路径用 Repo.InsertRun，同一事务里回填）。
func (r *RunRepo) Insert(ctx context.Context, run *model.NodeRun) error {
	_, err := r.db.NewInsert().Model(run).
		ExcludeColumn("created_at", "updated_at", "started_at", "finished_at", "task_id", "billing_request_id").Exec(ctx)
	return err
}

// ErrActiveOnNode 同一节点已有未结束的 run（uk_active_node 冲突）。
var ErrActiveOnNode = errors.New("repo: 该节点已有未结束的 run")

// IsActiveConflict 唯一键冲突是否来自 uk_active_node（同节点在途 run）。
func IsActiveConflict(err error) bool {
	return errors.Is(err, ErrActiveOnNode) || (IsDuplicate(err) && strings.Contains(err.Error(), "uk_active_node"))
}

// InsertRun 在一个事务里插入 run 并回填 task_id / billing_request_id（由主键算出）：行对其它连接可见时两者必然已有值，
// 幂等键与计费请求号不会退化。ids 由调用方按主键生成（billingRequestID 为空表示不计费）。
// uk_active_node 冲突返回 ErrActiveOnNode；uk_user_run 冲突原样返回（IsDuplicate=true，调用方走幂等重放）。
func (r *Repo) InsertRun(ctx context.Context, run *model.NodeRun, ids func(id int64) (taskID, billingRequestID string)) error {
	err := r.RunInTx(ctx, func(ctx context.Context, tx *Repo) error {
		if err := tx.Run.Insert(ctx, run); err != nil {
			return err
		}
		taskID, billReq := ids(run.ID)
		if err := tx.Run.BackfillTaskID(ctx, run.ID, taskID, billReq); err != nil {
			return err
		}
		run.TaskID.String, run.TaskID.Valid = taskID, true
		if billReq != "" {
			run.BillingRequestID.String, run.BillingRequestID.Valid = billReq, true
		}
		return nil
	})
	if err != nil && IsDuplicate(err) && strings.Contains(err.Error(), "uk_active_node") {
		return ErrActiveOnNode
	}
	return err
}

// BackfillTaskID 回填对外任务号与计费请求号（只填 task_id 为空的行）。
func (r *RunRepo) BackfillTaskID(ctx context.Context, id int64, taskID, billingRequestID string) error {
	q := r.db.NewUpdate().Table(TableRun).Set("task_id = ?", taskID)
	if billingRequestID != "" {
		q = q.Set("billing_request_id = ?", billingRequestID)
	}
	_, err := q.Where("id = ?", id).Where("task_id IS NULL").Exec(ctx)
	return err
}

// Get 按主键取。
func (r *RunRepo) Get(ctx context.Context, id int64) (*model.NodeRun, error) {
	x := new(model.NodeRun)
	return noRows(x, r.db.NewSelect().Model(x).Where("r.id = ?", id).Limit(1).Scan(ctx))
}

// GetByUserRun 按 (user_id, run_id) 取（幂等）。
func (r *RunRepo) GetByUserRun(ctx context.Context, userID int64, runID string) (*model.NodeRun, error) {
	x := new(model.NodeRun)
	return noRows(x, r.db.NewSelect().Model(x).Where("r.user_id = ?", userID).Where("r.run_id = ?", runID).Limit(1).Scan(ctx))
}

// ActiveOnNode 同一节点上未结束的 run（同一时刻只允许一个）。
func (r *RunRepo) ActiveOnNode(ctx context.Context, canvasID, nodeID string) (*model.NodeRun, error) {
	x := new(model.NodeRun)
	return noRows(x, r.db.NewSelect().Model(x).Where("r.canvas_id = ?", canvasID).Where("r.node_id = ?", nodeID).
		Where("r.status IN (?)", bun.In(model.RunActive)).OrderExpr("r.id DESC").Limit(1).Scan(ctx))
}

// LatestByCanvas 每个节点最近一次 run（任意状态；GET 画布回填节点状态用）。
func (r *RunRepo) LatestByCanvas(ctx context.Context, canvasID string) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	sub := r.db.NewSelect().Table(TableRun).ColumnExpr("MAX(id)").
		Where("canvas_id = ?", canvasID).Where("deleted = 0").GroupExpr("node_id")
	err := r.db.NewSelect().Model(&list).Where("r.id IN (?)", sub).Scan(ctx)
	return list, err
}

// LatestSucceededByNode 每个节点最近一次**成功**的 run（回填成品 / 候选、下游按 candidate_index 取图用）：
// 最近一次失败或在途时，节点仍能看到上一次成功的结果。
func (r *RunRepo) LatestSucceededByNode(ctx context.Context, canvasID string) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	sub := r.db.NewSelect().Table(TableRun).ColumnExpr("MAX(id)").
		Where("canvas_id = ?", canvasID).Where("deleted = 0").Where("status = ?", model.RunSucceeded).GroupExpr("node_id")
	err := r.db.NewSelect().Model(&list).Where("r.id IN (?)", sub).Scan(ctx)
	return list, err
}

// GetByCanvasRun 按 (user_id, canvas_id, node_id, run_id) 取（选候选用）。
func (r *RunRepo) GetByCanvasRun(ctx context.Context, userID int64, canvasID, nodeID, runID string) (*model.NodeRun, error) {
	x := new(model.NodeRun)
	return noRows(x, r.db.NewSelect().Model(x).Where("r.user_id = ?", userID).Where("r.canvas_id = ?", canvasID).
		Where("r.node_id = ?", nodeID).Where("r.run_id = ?", runID).Where("r.deleted = 0").Limit(1).Scan(ctx))
}

// SetSelected 写回选中的候选下标。
func (r *RunRepo) SetSelected(ctx context.Context, id int64, index int32) error {
	_, err := r.db.NewUpdate().Table(TableRun).Set("selected_index = ?", index).Where("id = ?", id).Exec(ctx)
	return err
}

// Touch 把仍停在 status 的这些 run 的 updated_at 刷成现在：ListByStatus 按 updated_at 升序取前 N 条，
// 处理过的排到队尾，避免长时间在途（或在等 Bearer）的 run 一直占着队头。
func (r *RunRepo) Touch(ctx context.Context, ids []int64, status int8) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.db.NewUpdate().Table(TableRun).Set("updated_at = NOW(3)").
		Where("id IN (?)", bun.In(ids)).Where("status = ?", status).Exec(ctx)
	return err
}

// NodesWithRuns 画布里有过 run 的节点 id（保存时剥离成品字段用）。
func (r *RunRepo) NodesWithRuns(ctx context.Context, canvasID string) (map[string]bool, error) {
	var ids []string
	err := r.db.NewSelect().Table(TableRun).Distinct().Column("node_id").
		Where("canvas_id = ?", canvasID).Where("deleted = 0").Scan(ctx, &ids)
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// DedupeCandidatesOnNode 智能体短窗口去重的候选：用户某画布某节点上仍在途（提交中 / 排队 / 运行中，不限时间）
// 或 created_at 在 succeededSince 之后已成功的 run，新的在前。
func (r *RunRepo) DedupeCandidatesOnNode(ctx context.Context, userID int64, canvasID, nodeID string, succeededSince time.Time, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Where("r.user_id = ?", userID).Where("r.canvas_id = ?", canvasID).
		Where("r.node_id = ?", nodeID).Where("r.deleted = 0").
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("r.status IN (?)", bun.In(model.RunActive)).
				WhereOr("r.status = ? AND r.created_at >= FROM_UNIXTIME(? / 1000)", model.RunSucceeded, succeededSince.UnixMilli())
		}).
		OrderExpr("r.id DESC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// RecentDurations 近期同类（kind + 模型组）成功 run 的耗时（秒，创建 → 完成），最近的在前，最多 limit 个。
// 走 idx_status(status, updated_at) 倒序扫，再按 kind / model_group 过滤（报价的预计耗时用，结果在进程内缓存）。
func (r *RunRepo) RecentDurations(ctx context.Context, kind model.RunKind, modelGroup string, since time.Time, limit int) ([]int, error) {
	var rows []struct {
		Sec int `bun:"sec"`
	}
	err := r.db.NewSelect().Table(TableRun).
		ColumnExpr("TIMESTAMPDIFF(SECOND, created_at, finished_at) AS sec").
		Where("status = ?", model.RunSucceeded).Where("updated_at >= FROM_UNIXTIME(? / 1000)", since.UnixMilli()).
		Where("kind = ?", string(kind)).Where("model_group = ?", modelGroup).
		Where("finished_at IS NOT NULL").Where("deleted = 0").
		OrderExpr("updated_at DESC").Limit(int64(limit)).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(rows))
	for _, x := range rows {
		if x.Sec > 0 {
			out = append(out, x.Sec)
		}
	}
	return out, nil
}

// ListByCanvas 用户某画布（可限节点）的 run，新的在前。
func (r *RunRepo) ListByCanvas(ctx context.Context, userID int64, canvasID, nodeID string, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	q := r.db.NewSelect().Model(&list).Where("r.user_id = ?", userID).Where("r.canvas_id = ?", canvasID).Where("r.deleted = 0")
	if nodeID != "" {
		q = q.Where("r.node_id = ?", nodeID)
	}
	err := q.OrderExpr("r.id DESC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ListByStatus 某状态的 run（轮询器用），按 updated_at、id 升序。只取没有租约或租约已过期的
// （别的实例正在处理的不列，减少空抢；能否处理以 AcquireLease 为准）。
func (r *RunRepo) ListByStatus(ctx context.Context, status int8, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Where("r.status = ?", status).Where(lease.FreeCond("r")).
		OrderExpr("r.updated_at ASC, r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ListHeldActive 需要续期预留单的 run：排队 / 提交中（派发阶段）/ 运行中、gateway 后端、计费镜像 held、有计费请求号。按 id 升序，最多 limit 条。
func (r *RunRepo) ListHeldActive(ctx context.Context, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Column("r.id", "r.user_id", "r.billing_request_id", "r.billing_status").
		Where("r.status IN (?)", bun.In([]int8{model.RunQueued, model.RunSubmitting, model.RunRunning})).
		Where("r.backend = ?", model.BackendGateway).
		Where("r.billing_status = ?", model.BillHeld).
		Where("r.billing_request_id IS NOT NULL AND r.billing_request_id <> ''").
		OrderExpr("r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ListFinalBillingOpen 计费镜像待对账的 run：已终态（成功 / 失败 / 取消）、gateway 后端、镜像仍是 review（UNKNOWN 待对账）、
// held 或 unsettled（已交付但确认扣费失败），有计费请求号，且 updated_at 不早于 since（走 idx_status(status, updated_at)
// 的范围扫描，不全表扫）。只取 id > afterID 的，按 id 升序，最多 limit 条：调用方记录上次游标轮转，积压时每轮都能前进，
// 不会被长期仍 HELD 的行占住队头。
func (r *RunRepo) ListFinalBillingOpen(ctx context.Context, since time.Time, afterID int64, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).
		Column("r.id", "r.user_id", "r.kind", "r.billing_request_id", "r.billing_status", "r.candidates").
		Where("r.status IN (?)", bun.In([]int8{model.RunSucceeded, model.RunFailed, model.RunCanceled})).
		Where("r.updated_at >= ?", since).
		Where("r.backend = ?", model.BackendGateway).
		Where("r.billing_status IN (?)", bun.In([]string{model.BillReview, model.BillHeld, model.BillUnsettled})).
		Where("r.billing_request_id IS NOT NULL AND r.billing_request_id <> ''").
		Where("r.id > ?", afterID).
		OrderExpr("r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// CASBillingStatus 仅当计费镜像仍是 from 时改成 to（与结算 / 退款 / 取消路径的写入互斥，谁先谁赢）。返回是否改到。
func (r *RunRepo) CASBillingStatus(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := r.db.NewUpdate().Table(TableRun).Set("billing_status = ?", to).
		Where("id = ?", id).Where("billing_status = ?", from).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AcquireLease 抢 run 的行租约（pkg/lease.Acquire）：同一 run 同一时刻只由一个 canvas-rpc 实例提交 / 轮询 / 收尾。
func (r *RunRepo) AcquireLease(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	return lease.Acquire(ctx, r.db, TableRun, id, owner, ttl)
}

// ReleaseLease 释放 run 的行租约（只对 owner 生效）。
func (r *RunRepo) ReleaseLease(ctx context.Context, id int64, owner string) error {
	return lease.Release(ctx, r.db, TableRun, id, owner)
}

// KeepLease 抢 run 的行租约并后台每 1/3 TTL 心跳续租（pkg/lease.Keep）；ok=false 表示别的实例持有。
// 返回的 ctx 在租约明确丢失（被接管）或一直续不上时取消；stop 停止心跳并释放租约（幂等）。
func (r *RunRepo) KeepLease(ctx context.Context, id int64, owner string, ttl time.Duration) (context.Context, func(), bool, error) {
	return lease.Keep(ctx, r.db, TableRun, id, owner, ttl)
}

// LeaseSet 一组 run 的租约（逐个抢 / 放，后台心跳批量续租）；用完 Close。
func (r *RunRepo) LeaseSet(owner string, ttl time.Duration) *lease.Set {
	return lease.NewSet(r.db, TableRun, owner, ttl)
}

// Transit CAS 迁移状态：进运行中补 started_at，进终态补 finished_at（extra 已给的不覆盖）。
func (r *RunRepo) Transit(ctx context.Context, id int64, from, to int8, extra map[string]any) (bool, error) {
	set := make(map[string]any, len(extra)+2)
	for k, v := range extra {
		set[k] = v
	}
	if _, ok := set["started_at"]; !ok && to == model.RunRunning {
		set["started_at"] = bun.Safe("NOW(3)")
	}
	if _, ok := set["finished_at"]; !ok && model.RunFinal(to) {
		set["finished_at"] = bun.Safe("NOW(3)")
	}
	if s, ok := set["error_reason"].(string); ok {
		set["error_reason"] = Trunc(s, 255)
	}
	return bundb.TransitStatus(ctx, r.db, TableRun, id, int(from), int(to), set)
}

// TransitUncanceled 同 Transit，但只在没有用户取消请求（cancel_request=0）且 attempt 仍是本方抢 run 时写下的值时迁移：
// 派发器提交后「转运行中」/「退回排队」都用它。与 RequestCancel 互斥——标记先落则迁移不到，由持有方按取消收尾；迁移先落则标记打不上，
// 取消走运行中路径。attempt 条件防旧持有方（提交太久、兜底退回排队后已被别的实例重新抢到）误改新持有方的 run。
func (r *RunRepo) TransitUncanceled(ctx context.Context, id int64, from, to int8, attempt int32, extra map[string]any) (bool, error) {
	return r.transitAttempt(ctx, id, from, to, attempt, true, extra)
}

// TransitAttempt 同 Transit，另加 attempt 条件（不看取消标记）：派发器 / 兜底在提交中收尾时用，防旧持有方误改新持有方的 run。
func (r *RunRepo) TransitAttempt(ctx context.Context, id int64, from, to int8, attempt int32, extra map[string]any) (bool, error) {
	return r.transitAttempt(ctx, id, from, to, attempt, false, extra)
}

// MarkSubmitUnconfirmed 提交以永久错误结束、但有提交结局未知（网关可能还没落库）：run 留在提交中，只记下待确认的失败原因
// （error_code = prefix + 失败码，error_reason = 文案），由提交中兜底过 submittingTTL 后按幂等键确认再收尾。条件含本方 attempt。
func (r *RunRepo) MarkSubmitUnconfirmed(ctx context.Context, id int64, attempt int32, code, reason string) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableRun).Set("error_code = ?", code).Set("error_reason = ?", Trunc(reason, 255)).
		Set("updated_at = NOW(3)").
		Where("id = ?", id).Where("status = ?", model.RunSubmitting).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

func (r *RunRepo) transitAttempt(ctx context.Context, id int64, from, to int8, attempt int32, uncanceled bool, extra map[string]any) (bool, error) {
	q := r.db.NewUpdate().Table(TableRun).Set("status = ?", to).Set("updated_at = NOW(3)")
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := extra[k]
		if s, ok := v.(string); ok && k == "error_reason" {
			v = Trunc(s, 255)
		}
		q = q.Set("? = ?", bun.Ident(k), v)
	}
	if _, ok := extra["started_at"]; !ok && to == model.RunRunning {
		q = q.Set("started_at = NOW(3)")
	}
	q = q.Where("id = ?", id).Where("status = ?", from)
	if uncanceled {
		q = q.Where("cancel_request = ?", model.CancelNone)
	}
	n, err := affected(q.Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// ClaimDispatch 派发器发请求前抢 run：排队 → 提交中，attempt+1（attempt>0 即「可能已向后端提交过」），条件是仍在排队、
// 没有用户取消请求、attempt 未被别处改过。返回是否抢到；没抢到由调用方重读区分「已请求取消」与「状态已变」。
func (r *RunRepo) ClaimDispatch(ctx context.Context, id int64, attempt int32) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableRun).Set("status = ?", model.RunSubmitting).Set("attempt = ?", attempt+1).
		Set("updated_at = NOW(3)").
		Where("id = ?", id).Where("status = ?", model.RunQueued).Where("cancel_request = ?", model.CancelNone).
		Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// RequestCancel 用户在排队 / 提交中取消：只打「已请求取消」标记，不改状态、不退款。只对仍在排队 / 提交中、还没有标记的行生效；
// 返回是否打上（已打过 / 状态已变返回 false，由调用方重读判断）。
func (r *RunRepo) RequestCancel(ctx context.Context, id int64) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableRun).Set("cancel_request = ?", model.CancelRequested).
		Where("id = ?", id).Where("status IN (?)", bun.In([]int8{model.RunQueued, model.RunSubmitting})).
		Where("cancel_request = ?", model.CancelNone).Exec(ctx))
	return n > 0, err
}

// ListStaleHolding 冻结阶段卡住的提交中 run（attempt=0、创建早于 before），按 id 升序。
func (r *RunRepo) ListStaleHolding(ctx context.Context, before time.Time, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Where("r.status = ?", model.RunSubmitting).Where("r.created_at < ?", before).
		Where("r.attempt = 0").OrderExpr("r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ListStaleDispatching 派发阶段卡住的提交中 run（attempt>0、updated_at 早于 before、没有租约或租约已过期：派发器宕机 / 早已放手），
// 按 id 升序。
func (r *RunRepo) ListStaleDispatching(ctx context.Context, before time.Time, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Where("r.status = ?", model.RunSubmitting).Where("r.attempt > 0").
		Where("r.updated_at < ?", before).Where(lease.FreeCond("r")).OrderExpr("r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// Update 不改状态地更新若干列（如 attempt / progress / backend_ref）。
func (r *RunRepo) Update(ctx context.Context, id int64, set map[string]any) error {
	if len(set) == 0 {
		return nil
	}
	_, err := bundb.UpdateIf(ctx, r.db, (*model.NodeRun)(nil), set, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ?", id)
	})
	return err
}

// UpdateIfStatus 仅当 run 仍处于 status 时更新若干列（不改状态），返回是否命中。
func (r *RunRepo) UpdateIfStatus(ctx context.Context, id int64, status int8, set map[string]any) (bool, error) {
	if s, ok := set["error_reason"].(string); ok {
		set["error_reason"] = Trunc(s, 255)
	}
	return bundb.UpdateIf(ctx, r.db, (*model.NodeRun)(nil), set, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ?", id).Where("status = ?", status)
	})
}

// SwapRefs 转存完成：把成功 run 的候选 / 视频地址换成素材库地址（只改 status=成功 的行；videoURL 为空不动视频列）。
// 顺手清掉转存失败标记（之前某轮记过永久失败、后来人工重置重做成功时）。
func (r *RunRepo) SwapRefs(ctx context.Context, id int64, candidates json.RawMessage, videoURL string) (bool, error) {
	q := r.db.NewUpdate().Table(TableRun).Set("candidates = ?", nullJSON(candidates))
	if videoURL != "" {
		q = q.Set("video_url = ?", videoURL)
	}
	q = q.Set("error_code = IF(error_code = ?, NULL, error_code)", model.CopyFailedCode)
	n, err := affected(q.Where("id = ?", id).Where("status = ?", model.RunSucceeded).Exec(ctx))
	return n > 0, err
}

// MarkCopyFailed 转存永久失败：在成功 run 上打 error_code 标记（library_status=failed），产物仍是 mr:// 引用、照常可看可下载。
func (r *RunRepo) MarkCopyFailed(ctx context.Context, id int64) error {
	_, err := r.db.NewUpdate().Table(TableRun).Set("error_code = ?", model.CopyFailedCode).
		Where("id = ?", id).Where("status = ?", model.RunSucceeded).Exec(ctx)
	return err
}

// ClearCopyFailed 用户重试转存：去掉成功 run 上的转存失败标记（library_status 回到 pending）。与 CopyJob.ResetFailed 同事务。
func (r *RunRepo) ClearCopyFailed(ctx context.Context, id int64) error {
	_, err := r.db.NewUpdate().Table(TableRun).Set("error_code = NULL").
		Where("id = ?", id).Where("status = ?", model.RunSucceeded).Where("error_code = ?", model.CopyFailedCode).Exec(ctx)
	return err
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// AdminFilter 后台列表筛选。
type AdminFilter struct {
	UserID         int64
	CanvasID, Kind string
	Backend        string
	Status         int32 // -1 全部
	StartMs, EndMs int64
	Offset, Limit  int64
}

func (f AdminFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f.UserID > 0 {
		q = q.Where("r.user_id = ?", f.UserID)
	}
	if f.CanvasID != "" {
		q = q.Where("r.canvas_id = ?", f.CanvasID)
	}
	if f.Kind != "" {
		q = q.Where("r.kind = ?", f.Kind)
	}
	if f.Backend != "" {
		q = q.Where("r.backend = ?", f.Backend)
	}
	if f.Status >= 0 {
		q = q.Where("r.status = ?", f.Status)
	}
	if f.StartMs > 0 {
		q = q.Where("r.created_at >= "+msExpr, f.StartMs)
	}
	if f.EndMs > 0 {
		q = q.Where("r.created_at < "+msExpr, f.EndMs)
	}
	return q
}

// AdminList 后台 run 列表。
func (r *RunRepo) AdminList(ctx context.Context, f AdminFilter) ([]*model.NodeRun, int64, error) {
	var list []*model.NodeRun
	q := f.apply(r.db.NewSelect().Model(&list)).OrderExpr("r.id DESC").Offset(f.Offset).Limit(f.Limit)
	n, err := q.ScanAndCount(ctx)
	return list, int64(n), err
}

// CostRow 成本汇总一行。
type CostRow struct {
	Key       string `bun:"k"`
	Runs      int64  `bun:"runs"`
	Succeeded int64  `bun:"succeeded"`
	Failed    int64  `bun:"failed"`
	Canceled  int64  `bun:"canceled"`
	Cost      string `bun:"cost"`
	Points    int64  `bun:"points"`
}

// CostSummary 按 kind 或 model_group 汇总成本（只读）。
// TODO(sqlc)：设计 §9.5 定成本汇总走 pkg/auditdb；与 studio 同口径，先在这里用 SUM。
func (r *RunRepo) CostSummary(ctx context.Context, startMs, endMs int64, groupBy string) ([]CostRow, error) {
	col := "r.kind"
	if strings.EqualFold(groupBy, "model_group") {
		col = "r.model_group"
	}
	var rows []CostRow
	q := r.db.NewSelect().Model((*model.NodeRun)(nil)).
		ColumnExpr(col + " AS k").
		ColumnExpr("COUNT(*) AS runs").
		ColumnExpr("SUM(r.status = 2) AS succeeded").
		ColumnExpr("SUM(r.status = 3) AS failed").
		ColumnExpr("SUM(r.status = 4) AS canceled").
		ColumnExpr("CAST(COALESCE(SUM(r.cost_cny), 0) AS CHAR) AS cost").
		ColumnExpr("COALESCE(SUM(CASE WHEN r.status = 2 THEN r.points ELSE 0 END), 0) AS points")
	q = AdminFilter{Status: -1, StartMs: startMs, EndMs: endMs}.apply(q)
	err := q.GroupExpr(col).OrderExpr("k ASC").Scan(ctx, &rows)
	return rows, err
}

// MarshalCandidates 候选 → JSON（空 → nil，写 NULL）。
func MarshalCandidates(c []model.Candidate) json.RawMessage {
	if len(c) == 0 {
		return nil
	}
	b, _ := json.Marshal(c)
	return b
}

// ListSettledFinished 付费产物入库对账（logic/asset_reconcile.go）：成功、已确认扣费（billing_status=settled）、kind ∈ kinds 的 run，
// 附加键集分页条件 cond（pkg/asset.KeysetCond 生成，列为 r.updated_at / r.id），按 updated_at, id 升序（走 idx_sync_updated_id）。
// 不看 deleted：用户删除画布 / run 不影响付费产物入素材库（与转存 outbox 同一口径）。
func (r *RunRepo) ListSettledFinished(ctx context.Context, kinds []model.RunKind, cond string, args []any, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Where("r.status = ?", model.RunSucceeded).Where("r.billing_status = ?", model.BillSettled).
		Where("r.kind IN (?)", bun.In(kinds)).Where(cond, args...).
		OrderExpr("r.updated_at ASC, r.id ASC").Limit(int64(max(1, limit))).Scan(ctx)
	return list, err
}
