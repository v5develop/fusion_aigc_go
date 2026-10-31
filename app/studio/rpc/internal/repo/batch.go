package repo

import (
	"context"
	"encoding/json"
	"errors"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/lease"

	"github.com/uptrace/bun"
)

// BatchRepo afv_studio_batch。
type BatchRepo struct{ db bun.IDB }

// Insert 插入批次，回填 b.ID（自增主键）。应在事务里与 Task.BulkInsert 一起调。
func (r *BatchRepo) Insert(ctx context.Context, b *model.Batch) error {
	_, err := r.db.NewInsert().Model(b).Exec(ctx)
	return err
}

// GetByID 取批次。userID>0 时限定本人且未删除（用户侧）；userID=0 不限（后台 / 轮询器，含已删除）。
func (r *BatchRepo) GetByID(ctx context.Context, id, userID int64) (*model.Batch, error) {
	b := new(model.Batch)
	q := r.db.NewSelect().Model(b).Where("b.id = ?", id)
	if userID > 0 {
		q = q.Where("b.user_id = ?", userID).Where("b.deleted = 0")
	}
	return noRows(b, q.Limit(1).Scan(ctx))
}

// GetByUserRequest 幂等查询：按 uk_user_request(user_id, request_id) 取（含已删除，删了也算用过这个键）。
func (r *BatchRepo) GetByUserRequest(ctx context.Context, userID int64, requestID string) (*model.Batch, error) {
	b := new(model.Batch)
	err := r.db.NewSelect().Model(b).Where("b.user_id = ?", userID).Where("b.request_id = ?", requestID).Limit(1).Scan(ctx)
	return noRows(b, err)
}

// UpdateStatus CAS 迁移批次状态：WHERE id=? AND status=from。extra 为同时更新的列（不得含 status / updated_at）。
func (r *BatchRepo) UpdateStatus(ctx context.Context, id int64, from, to int8, extra map[string]any) (bool, error) {
	return bundb.TransitStatus(ctx, r.db, TableBatch, id, int(from), int(to), extra)
}

// UpdateCounters task_done / task_failed 自增（可为 0；不做 CAS，调用方保证每个子任务只计一次）。
func (r *BatchRepo) UpdateCounters(ctx context.Context, id int64, doneDelta, failedDelta int) error {
	if doneDelta == 0 && failedDelta == 0 {
		return nil
	}
	_, err := r.db.NewUpdate().Table(TableBatch).
		Set("task_done = task_done + ?", doneDelta).
		Set("task_failed = task_failed + ?", failedDelta).
		Where("id = ?", id).Exec(ctx)
	return err
}

// SetCropPlan 写切图方案（JSON）。
func (r *BatchRepo) SetCropPlan(ctx context.Context, id int64, plan json.RawMessage) error {
	return r.setJSON(ctx, id, "crop_plan", plan)
}

// SetCropCheck 写切图质检结果（JSON）。
func (r *BatchRepo) SetCropCheck(ctx context.Context, id int64, check json.RawMessage) error {
	return r.setJSON(ctx, id, "crop_check", check)
}

// SetSelected 写组图结果（JSON，[]model.SelectedImages）。
func (r *BatchRepo) SetSelected(ctx context.Context, id int64, selected json.RawMessage) error {
	return r.setJSON(ctx, id, "selected_images", selected)
}

// SetEstimatedPoints 写预估点数（提交时各子任务冻结点数之和）。
func (r *BatchRepo) SetEstimatedPoints(ctx context.Context, id, points int64) error {
	_, err := r.db.NewUpdate().Table(TableBatch).Set("estimated_points = ?", points).Where("id = ?", id).Exec(ctx)
	return err
}

// SetSourceSha1 写姿态人物图 SHA-1（编排下载人物图后回填，同图复用 source 反推缓存）。
func (r *BatchRepo) SetSourceSha1(ctx context.Context, id int64, sha1 string) error {
	_, err := r.db.NewUpdate().Table(TableBatch).Set("source_sha1 = ?", sha1).Where("id = ?", id).Exec(ctx)
	return err
}

// SetError 写批次级失败原因（截到 255 字）。
func (r *BatchRepo) SetError(ctx context.Context, id int64, reason string) error {
	_, err := r.db.NewUpdate().Table(TableBatch).Set("error_reason = ?", Trunc(reason, 255)).Where("id = ?", id).Exec(ctx)
	return err
}

func (r *BatchRepo) setJSON(ctx context.Context, id int64, col string, v json.RawMessage) error {
	if len(v) == 0 {
		return errors.New("repo: " + col + " 不能为空")
	}
	if !json.Valid(v) {
		return errors.New("repo: " + col + " 不是合法 JSON")
	}
	_, err := r.db.NewUpdate().Table(TableBatch).
		Set("? = CAST(? AS JSON)", bun.Ident(col), string(v)).
		Where("id = ?", id).Exec(ctx)
	return err
}

// AppendStepLog 往 step_log 数组末尾追加一条（JSON_ARRAY_APPEND，step_log 为 NULL 时从空数组开始）。
func (r *BatchRepo) AppendStepLog(ctx context.Context, id int64, entry model.StepLog) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = r.db.NewUpdate().Table(TableBatch).
		Set("step_log = JSON_ARRAY_APPEND(COALESCE(step_log, JSON_ARRAY()), '$', CAST(? AS JSON))", string(raw)).
		Where("id = ?", id).Exec(ctx)
	return err
}

// Cursor 历史游标：(created_at 毫秒, id)。零值 = 第一页。
type Cursor struct {
	Ms int64
	ID int64
}

// ListHistory 用户历史：user_id + 未删除 [+ kind]，按 (created_at, id) 倒序，游标之后取 limit 条（走 idx_user_created）。
// 调用方想判断有没有下一页就传 limit+1。
func (r *BatchRepo) ListHistory(ctx context.Context, userID int64, kind model.Kind, cur Cursor, limit int) ([]*model.Batch, error) {
	var list []*model.Batch
	q := r.db.NewSelect().Model(&list).Where("b.user_id = ?", userID).Where("b.deleted = 0")
	if kind != "" {
		q = q.Where("b.kind = ?", kind)
	}
	if cur.Ms > 0 && cur.ID > 0 {
		q = q.Where("(b.created_at < "+msExpr+" OR (b.created_at = "+msExpr+" AND b.id < ?))", cur.Ms, cur.Ms, cur.ID)
	}
	err := q.OrderExpr("b.created_at DESC, b.id DESC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// SoftDelete 软删本人的批次（deleted=0 → 1）。返回 false 表示不存在或已删。子任务另调 Task.SoftDeleteByBatch。
func (r *BatchRepo) SoftDelete(ctx context.Context, id, userID int64) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableBatch).Set("deleted = 1").
		Where("id = ?", id).Where("user_id = ?", userID).Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// ListByStatus 某状态的批次（轮询器用），按 updated_at、id 升序（最久没动的先推进），最多 limit 条。
// 只取没有租约或租约已过期的（别的实例正在推进的不列，减少空抢；是否真能推进以 Acquire 为准）。
func (r *BatchRepo) ListByStatus(ctx context.Context, status int8, limit int) ([]*model.Batch, error) {
	var list []*model.Batch
	err := r.db.NewSelect().Model(&list).Where("b.status = ?", status).Where(lease.FreeCond("b")).
		OrderExpr("b.updated_at ASC, b.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// ListStaleSubmitting 停在 提交中 且 created_at 早于 beforeMs（unix 毫秒）的批次：提交进程中途崩溃的兜底。
func (r *BatchRepo) ListStaleSubmitting(ctx context.Context, beforeMs int64, limit int) ([]*model.Batch, error) {
	var list []*model.Batch
	err := r.db.NewSelect().Model(&list).Where("b.status = ?", model.BatchSubmitting).
		Where("b.created_at < "+msExpr, beforeMs).
		OrderExpr("b.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// CountByStatuses 处于这些状态的批次数（后台依赖面板：排队 / 切图 / 反推中）。
func (r *BatchRepo) CountByStatuses(ctx context.Context, statuses ...int8) (int64, error) {
	return r.db.NewSelect().Model((*model.Batch)(nil)).Where("b.status IN (?)", bun.In(statuses)).Count(ctx)
}

// AdminFilter 后台批次列表筛选。零值不限；Status<0 不限；时间为 unix 毫秒，左闭右开。
type AdminFilter struct {
	UserID  int64
	Kind    model.Kind
	Status  int8
	StartMs int64
	EndMs   int64
}

// AdminList 后台分页（含已删除），按 id 倒序。page 从 1 起。
func (r *BatchRepo) AdminList(ctx context.Context, f AdminFilter, page, size int) ([]*model.Batch, int64, error) {
	var list []*model.Batch
	q := r.db.NewSelect().Model(&list)
	if f.UserID > 0 {
		q = q.Where("b.user_id = ?", f.UserID)
	}
	if f.Kind != "" {
		q = q.Where("b.kind = ?", f.Kind)
	}
	if f.Status >= 0 {
		q = q.Where("b.status = ?", f.Status)
	}
	if f.StartMs > 0 {
		q = q.Where("b.created_at >= "+msExpr, f.StartMs)
	}
	if f.EndMs > 0 {
		q = q.Where("b.created_at < "+msExpr, f.EndMs)
	}
	if page < 1 {
		page = 1
	}
	total, err := q.OrderExpr("b.id DESC").Limit(int64(size)).Offset(int64((page - 1) * size)).ScanAndCount(ctx)
	return list, total, err
}

// Trunc 按字符截断（varchar(255) 之类按字符计长度）。
func Trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
