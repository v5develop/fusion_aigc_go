package repo

import (
	"context"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// TableCopyJob 产物转存素材库 outbox（Go_V1.0.29）。
const TableCopyJob = "afv_canvas_copy_job"

// CopyJobRepo afv_canvas_copy_job。时间一律用库时钟（NOW(3)），多实例之间不受本机时钟漂移影响。
//
// 认领协议：attempt 是乐观锁。Claim 以 (id, status=0, attempt=旧值) 为条件把 attempt+1 并把 next_at 推到认领超时之后，
// 抢到的实例独占这一轮；之后的 Retry / Finish 都带 attempt=新值，认领超时被别的实例接手（attempt 又+1）后旧持有者的写入落空。
type CopyJobRepo struct{ db bun.IDB }

// Enqueue 落一行待转存（同一 run 已有行时什么都不做：INSERT IGNORE 撞 uk_run）。delaySec 后才对扫描可见，
// 留给本进程的即时转存（见 logic/library_copy.go 的 Kick），进程崩溃时由扫描接手。
func (r *CopyJobRepo) Enqueue(ctx context.Context, runPK int64, taskID string, userID int64, delaySec int) error {
	_, err := r.db.NewInsert().Model(&model.CopyJob{RunPK: runPK, TaskID: taskID, UserID: userID, Status: model.CopyPending}).
		ExcludeColumn("id", "created_at", "updated_at", "finished_at", "last_error").
		Value("next_at", "NOW(3) + INTERVAL ? SECOND", delaySec).
		Ignore().Exec(ctx)
	return err
}

// GetByRun 按 run 主键取（没有返回 nil, nil）。
func (r *CopyJobRepo) GetByRun(ctx context.Context, runPK int64) (*model.CopyJob, error) {
	x := new(model.CopyJob)
	return noRows(x, r.db.NewSelect().Model(x).Where("j.run_pk = ?", runPK).Limit(1).Scan(ctx))
}

// ListDue 到点的待转存行（next_at 升序）。
func (r *CopyJobRepo) ListDue(ctx context.Context, limit int) ([]*model.CopyJob, error) {
	var list []*model.CopyJob
	err := r.db.NewSelect().Model(&list).Where("j.status = ?", model.CopyPending).Where("j.next_at <= NOW(3)").
		OrderExpr("j.next_at ASC, j.id ASC").Limit(int64(limit)).Scan(ctx)
	return list, err
}

// Claim 认领一轮：attempt CAS +1，next_at 推到 leaseSec 之后（本轮没在这段时间内写回结果就会被扫描接手）。
func (r *CopyJobRepo) Claim(ctx context.Context, id int64, attempt int32, leaseSec int) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("attempt = attempt + 1").Set("next_at = NOW(3) + INTERVAL ? SECOND", leaseSec).
		Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// Renew 确认仍持有本轮认领并把认领续到 leaseSec 之后。返回 false 表示认领已被接手（或已进终态），调用方不应再写。
// next_at 每次至少推后 1ms：MySQL 的影响行数只算值真变了的行，同一毫秒内连续续期若写入同值会被误判成「认领已丢」。
func (r *CopyJobRepo) Renew(ctx context.Context, id int64, attempt int32, leaseSec int) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("next_at = GREATEST(NOW(3) + INTERVAL ? SECOND, next_at + INTERVAL 1000 MICROSECOND)", leaseSec).
		Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// Retry 本轮失败，delaySec 后再试（只对仍持有本轮认领的生效）。
func (r *CopyJobRepo) Retry(ctx context.Context, id int64, attempt int32, delaySec int, reason string) error {
	_, err := r.db.NewUpdate().Table(TableCopyJob).
		Set("next_at = NOW(3) + INTERVAL ? SECOND", delaySec).Set("last_error = ?", Trunc(reason, 255)).
		Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx)
	return err
}

// Finish 进终态（完成 / 永久失败 / 作废；只对仍持有本轮认领的生效）。返回是否写到。
func (r *CopyJobRepo) Finish(ctx context.Context, id int64, attempt int32, status int8, reason string) (bool, error) {
	q := r.db.NewUpdate().Table(TableCopyJob).Set("status = ?", status).Set("finished_at = NOW(3)")
	if reason != "" {
		q = q.Set("last_error = ?", Trunc(reason, 255))
	}
	n, err := affected(q.Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}
