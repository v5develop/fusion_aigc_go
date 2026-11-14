package repo

import (
	"context"
	"database/sql"
	"errors"

	"aigc-local-life/app/canvas/model"

	"github.com/go-sql-driver/mysql"
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
//
// snapshot 写进 src_snapshot（Go_V1.0.31）：入素材库所需的 run 字段，run 行被硬删除时据此兜底转存。列还不存在（迁移未执行）时
// 去掉这一列重写一次——MySQL 的 1054 只回滚这一条语句、不影响所在事务，所以在置成功的事务里重试是安全的，行为退回改造前。
func (r *CopyJobRepo) Enqueue(ctx context.Context, runPK int64, taskID string, userID int64, delaySec int, snapshot []byte) error {
	insert := func(withSnapshot bool) error {
		q := r.db.NewInsert().Model(&model.CopyJob{RunPK: runPK, TaskID: taskID, UserID: userID, Status: model.CopyPending}).
			ExcludeColumn("id", "created_at", "updated_at", "finished_at", "last_error").
			Value("next_at", "NOW(3) + INTERVAL ? SECOND", delaySec)
		if withSnapshot {
			q = q.Value("src_snapshot", "?", string(snapshot))
		}
		_, err := q.Ignore().Exec(ctx)
		return err
	}
	withSnapshot := len(snapshot) > 0
	err := insert(withSnapshot)
	if err != nil && withSnapshot && IsUnknownColumn(err) {
		err = insert(false)
	}
	return err
}

// SourceSnapshot 行的 src_snapshot（没有 / 列还不存在返回 nil, nil）。只在 run 行已不存在时读，常规路径不碰这一列，
// 所以迁移未执行时扫描 / 认领照常工作。
func (r *CopyJobRepo) SourceSnapshot(ctx context.Context, id int64) ([]byte, error) {
	var v sql.NullString
	err := r.db.NewSelect().Table(TableCopyJob).Column("src_snapshot").Where("id = ?", id).Limit(1).Scan(ctx, &v)
	switch {
	case errors.Is(err, sql.ErrNoRows), IsUnknownColumn(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	return []byte(v.String), nil
}

// IsUnknownColumn MySQL 1054（列不存在：新增列的迁移还没执行）。
func IsUnknownColumn(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1054
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

// CopyAttemptEpoch 用户重试转存时 attempt 跳到的代际步长：ResetFailed 把 attempt 推到下一个整千（1000、2000…），
// 而不是清零。认领超时被接手的旧持有者手里还拿着旧 attempt，若重置为 0、新一轮又数回同一个值，旧持有者的
// Renew / Finish / 换地址会重新「对上号」与新持有者并行写（素材可能入两条）；attempt 只增不减就永远对不上。
// 本代第几次尝试用 CopyRound 取（attempt % 1000），退避与 MaxAttempts 都按它算。
const CopyAttemptEpoch int32 = 1000

// CopyRound 本代第几次尝试（1 起；未认领为 0）。
func CopyRound(attempt int32) int32 { return attempt % CopyAttemptEpoch }

// ResetFailed 用户重试转存：永久失败的行重置为待转存（attempt 推到下一代 = 下一个整千、next_at=NOW(3) 立即可被认领、
// finished_at 清空；last_error 保留供排障）。冷却：上次进终态（finished_at）不足 cooldownSec 秒的不重置——每次重置至少要
// 跑完一轮才可能再失败，所以同一 run 至多约每 cooldownSec 秒重置一次。返回是否改到（false = 不是永久失败 / 冷却中 /
// 并发的另一次已重置）。attempt 只增不减：旧代的持有者（认领超时后仍在跑的协程）写入一律落空，见 CopyAttemptEpoch。
func (r *CopyJobRepo) ResetFailed(ctx context.Context, id int64, cooldownSec int) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("status = ?", model.CopyPending).Set("attempt = (attempt DIV ? + 1) * ?", CopyAttemptEpoch, CopyAttemptEpoch).
		Set("next_at = NOW(3)").Set("finished_at = NULL").
		Where("id = ?", id).Where("status = ?", model.CopyFailed).
		Where("(finished_at IS NULL OR finished_at <= NOW(3) - INTERVAL ? SECOND)", cooldownSec).Exec(ctx))
	return n > 0, err
}

// FailWithRunMark 永久失败：outbox 行进终态（attempt CAS，只有本轮持有者写得到）与成功 run 打转存失败标记
// （library_status=failed）在同一事务里。返回是否写到（false = 认领已被接手 / 已进终态，什么都没改）。
// 二者同生同灭：旧做法先打标记再写终态，认领已丢的旧持有者也会把标记打上，而用户重试看到「行在转存中 + 有标记」
// 只清标记，若随后持有者写了终态，就会留下「行已永久失败、run 显示转存中」且再也无法重试的状态。
func (r *Repo) FailWithRunMark(ctx context.Context, jobID, runPK int64, attempt int32, reason string) (bool, error) {
	won := false
	err := r.RunInTx(ctx, func(ctx context.Context, tx *Repo) error {
		ok, err := tx.CopyJob.Finish(ctx, jobID, attempt, model.CopyFailed, reason)
		if err != nil || !ok {
			return err
		}
		if err := tx.Run.MarkCopyFailed(ctx, runPK); err != nil {
			return err
		}
		won = true
		return nil
	})
	return won && err == nil, err
}

// ListRequeueable 定时重投的候选：永久失败、代数未到 maxGen（attempt < maxGen * CopyAttemptEpoch）、且已过本代重投等待
// （dueCond / dueArgs 由 pkg/asset.RequeueDueCond 生成），finished_at 升序。
func (r *CopyJobRepo) ListRequeueable(ctx context.Context, maxGen int, dueCond string, dueArgs []any, limit int) ([]*model.CopyJob, error) {
	var list []*model.CopyJob
	err := r.db.NewSelect().Model(&list).Where("j.status = ?", model.CopyFailed).
		Where("j.attempt < ?", int64(maxGen)*int64(CopyAttemptEpoch)).Where(dueCond, dueArgs...).
		OrderExpr("j.finished_at ASC, j.id ASC").Limit(int64(max(1, limit))).Scan(ctx)
	return list, err
}

// Requeue 定时重投一行永久失败：同 ResetFailed（attempt 进下一代、next_at=NOW(3)、清 finished_at），但以列出时的 attempt 做 CAS——
// 与用户一键重试 / 别的实例的重投并发时只有一个改到（另一方 attempt 已变）。返回是否改到。
func (r *CopyJobRepo) Requeue(ctx context.Context, id int64, attempt int32) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("status = ?", model.CopyPending).Set("attempt = (attempt DIV ? + 1) * ?", CopyAttemptEpoch, CopyAttemptEpoch).
		Set("next_at = NOW(3)").Set("finished_at = NULL").
		Where("id = ?", id).Where("status = ?", model.CopyFailed).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}
