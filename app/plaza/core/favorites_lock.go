package core

import (
	"context"
	"errors"

	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// 用户维度的串行化（收藏 / 我的模板的「数量上限」检查）：
//   INSERT … SELECT … WHERE COUNT(*) < N 在 RR 下并发会因间隙锁互相等待而死锁（500），在 RC 下又可能一起通过而超限。
//   做法：每个用户在 afv_plaza_user_lock 里有一行（autocommit 的 INSERT IGNORE 先保证它存在，不在事务里抢插入锁），
//   事务里先 SELECT … FOR UPDATE 锁这一行（只锁已存在的记录，无间隙锁）→ 同一用户的计数 + 插入严格串行，之后再普通计数、插入。
//   锁住之后才发起第一次一致性读，所以计数能看到前一个事务已提交的行。
//   真遇到死锁（MySQL 1213）或锁等待超时（1205）重试 2 次，仍失败返回 429 TOO_FREQUENT（可重试），不给裸 500。

const (
	mysqlDeadlock    = 1213
	mysqlLockTimeout = 1205
	lockRetries      = 2
	// reasonTooFrequent 与 pkg/v1wire.ReasonTooFrequent 同值（HTTP 429，前端提示「操作太快了，稍后再试」）
	reasonTooFrequent = "TOO_FREQUENT"
)

func isLockConflict(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == mysqlDeadlock || me.Number == mysqlLockTimeout)
}

func errBusyRetry() error {
	return xerr.NewReason(xerr.Conflict, reasonTooFrequent, "操作太快了，稍后再试")
}

// withUserLock 在持有该用户锁的事务里执行 fn；锁冲突重试，最终仍冲突 → 可重试错误。
func (s *Service) withUserLock(ctx context.Context, userID int64, fn func(ctx context.Context, tx bun.Tx) error) error {
	if _, err := s.db.ExecContext(ctx, "INSERT IGNORE INTO afv_plaza_user_lock (user_id) VALUES (?)", userID); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt <= lockRetries; attempt++ {
		err = bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			var uid int64
			if err := tx.NewRaw("SELECT user_id FROM afv_plaza_user_lock WHERE user_id = ? FOR UPDATE", userID).Scan(ctx, &uid); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
		if err == nil || !isLockConflict(err) {
			return err
		}
	}
	return errBusyRetry()
}
