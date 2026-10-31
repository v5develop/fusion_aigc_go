package repo

import (
	"context"
	"encoding/json"
	"errors"
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

// ListStale 某状态下创建早于 before 的 run（提交中兜底用），按 id 升序。
func (r *RunRepo) ListStale(ctx context.Context, status int8, before time.Time, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Where("r.status = ?", status).Where("r.created_at < ?", before).
		OrderExpr("r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
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

// ListHeldActive 需要续期预留单的 run：排队 / 运行中、gateway 后端、计费镜像 held、有计费请求号。按 id 升序，最多 limit 条。
func (r *RunRepo) ListHeldActive(ctx context.Context, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Column("r.id", "r.user_id", "r.billing_request_id", "r.billing_status").
		Where("r.status IN (?)", bun.In([]int8{model.RunQueued, model.RunRunning})).
		Where("r.backend = ?", model.BackendGateway).
		Where("r.billing_status = ?", model.BillHeld).
		Where("r.billing_request_id IS NOT NULL AND r.billing_request_id <> ''").
		OrderExpr("r.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ListFinalBillingOpen 计费镜像待对账的 run：已终态（成功 / 失败 / 取消）、gateway 后端、镜像仍是 review（UNKNOWN 待对账）或 held、
// 有计费请求号，且 updated_at 不早于 since（走 idx_status(status, updated_at) 的范围扫描，不全表扫）。按 id 升序，最多 limit 条。
func (r *RunRepo) ListFinalBillingOpen(ctx context.Context, since time.Time, limit int) ([]*model.NodeRun, error) {
	var list []*model.NodeRun
	err := r.db.NewSelect().Model(&list).Column("r.id", "r.user_id", "r.billing_request_id", "r.billing_status").
		Where("r.status IN (?)", bun.In([]int8{model.RunSucceeded, model.RunFailed, model.RunCanceled})).
		Where("r.updated_at >= ?", since).
		Where("r.backend = ?", model.BackendGateway).
		Where("r.billing_status IN (?)", bun.In([]string{model.BillReview, model.BillHeld})).
		Where("r.billing_request_id IS NOT NULL AND r.billing_request_id <> ''").
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
