package repo

import (
	"context"
	"database/sql"
	"errors"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
)

// 换声音额度（Go_V1.0.57，CONTRACT §P）：每日计数 + 按任务的占用流水。
// 占用与计数在同一事务：流水唯一键（job_id）保证一个任务只占一次（确认重放不重复占），退回用「状态 1→2」的条件更新保证只退一次。

var _ core.VoiceStore = (*Repo)(nil)

var errVoiceQuotaFull = errors.New("replicate: 换声音额度已满")

// AcquireVoiceQuota 给任务占一次当日额度：流水已存在（重放）直接算占到、不再 +1；否则「used < total 才 +1」，额度满回滚整个事务。
func (r *Repo) AcquireVoiceQuota(ctx context.Context, userID, jobID int64, day string, total int) (int, bool, error) {
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT IGNORE INTO `"+model.TableVoiceUse+"` (`job_id`, `user_id`, `day`, `status`) VALUES (?, ?, ?, ?)",
			jobID, userID, day, model.VoiceUseHeld)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 这个任务已经占过了（确认重放）
		}
		if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO `"+model.TableVoiceQuota+"` (`user_id`, `day`, `used`) VALUES (?, ?, 0)", userID, day); err != nil {
			return err
		}
		ok, err := one(tx.NewUpdate().Table(model.TableVoiceQuota).Set("used = used + 1").
			Where("user_id = ? AND day = ? AND used < ?", userID, day, total).Exec(ctx))
		if err != nil {
			return err
		}
		if !ok {
			return errVoiceQuotaFull // 回滚：流水一并撤销
		}
		return nil
	})
	if errors.Is(err, errVoiceQuotaFull) {
		used, uerr := r.VoiceQuotaUsed(ctx, userID, day)
		return used, false, uerr
	}
	if err != nil {
		return 0, false, err
	}
	used, err := r.VoiceQuotaUsed(ctx, userID, day)
	return used, true, err
}

// ReleaseVoiceQuota 退回任务占用的那一次额度（扣回占用时的那一天）。没占过 / 已退过 = false，不报错。
func (r *Repo) ReleaseVoiceQuota(ctx context.Context, jobID int64) (bool, error) {
	released := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		ok, err := one(tx.NewUpdate().Table(model.TableVoiceUse).Set("status = ?", model.VoiceUseRefunded).
			Where("job_id = ? AND status = ?", jobID, model.VoiceUseHeld).Exec(ctx))
		if err != nil || !ok {
			return err
		}
		var u model.VoiceUse
		if err := tx.NewSelect().Model(&u).Where("pvu.job_id = ?", jobID).Limit(1).Scan(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Table(model.TableVoiceQuota).Set("used = used - 1").
			Where("user_id = ? AND day = ? AND used > 0", u.UserID, u.Day).Exec(ctx); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

// VoiceQuotaUsed 当日已占用条数。
func (r *Repo) VoiceQuotaUsed(ctx context.Context, userID int64, day string) (int, error) {
	var used int
	err := r.db.NewSelect().Table(model.TableVoiceQuota).Column("used").Where("user_id = ? AND day = ?", userID, day).Limit(1).Scan(ctx, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}
