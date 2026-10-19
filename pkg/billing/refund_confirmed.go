package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RefundConfirmed 退回已确认（已扣费）的预留单：把它实际消费掉的点数全部退回余额。
//
// 只用于人工处理这类「点数已扣、结果未落库」的单子，自动路径不会调用（自动路径只退冻结中的 HELD）。
// 口径与 releaseFrozen 的「退回」一致：退回原批次（remaining 加回），批次置回 ACTIVE，expire_time 不变；
// 账户：balance 加回，total_consumed 减掉，total_refunded 加上；流水记一条 REFUND（幂等键 post-refund:{no}）。
//
// 返回本次退回的点数。已退回过（REFUNDED）返回 0，重复调用安全；没有预留单也返回 0。
func (s *Service) RefundConfirmed(ctx context.Context, userID int64, requestID, reason string) (int64, error) {
	if s.conf.Mode != ModeOn || requestID == "" {
		return 0, nil
	}
	bizID := Charge{UserID: userID, RequestID: requestID}.bizID()
	var refunded int64
	err := s.conn.TransactCtx(ctx, func(ctx context.Context, sess sqlx.Session) error {
		var no string
		switch err := sess.QueryRowCtx(ctx, &no,
			"SELECT `reservation_no` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 LIMIT 1",
			bizTypeLocalLife, bizID); {
		case errors.Is(err, sqlx.ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		res, err := lockReservation(ctx, sess, no)
		if err != nil {
			return err
		}
		switch res.Status {
		case statusRefunded:
			return nil // 幂等：已经退过
		case statusConfirmed, statusPartiallyRefunded:
		default:
			return fmt.Errorf("%w: %s 当前是 %s，只能退回已确认的单子", ErrGone, no, res.Status)
		}

		// 本单实际消费了哪些批次、多少点：来自它的 CONSUME 流水明细（确认时写下的）
		var consumed []struct {
			BatchID int64 `db:"batch_id"`
			Amount  int64 `db:"amount"`
		}
		const qc = "SELECT ll.`batch_id`, -ll.`amount` AS `amount` FROM `afv_points_log` pl " +
			"JOIN `afv_points_log_line` ll ON ll.`ledger_id` = pl.`id` " +
			"WHERE pl.`user_id` = ? AND pl.`change_type` = ? AND pl.`idempotency_key` IN (?, ?)"
		if err := sess.QueryRowsCtx(ctx, &consumed, qc, res.UserId, changeConsume,
			"confirm:"+no, "partial-consume:"+no); err != nil {
			return err
		}
		perBatch := map[int64]int64{}
		var total int64
		for _, c := range consumed {
			perBatch[c.BatchID] += c.Amount
			total += c.Amount
		}
		if total <= 0 {
			// 已确认却找不到消费流水：不知道点数落在哪些批次，拒绝退回，避免凭空加钱
			return fmt.Errorf("billing: %s 已确认但找不到消费流水，拒绝退回，请人工核对", no)
		}

		var a Account
		const lockAcc = "SELECT " + accountCols + " FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
		if err := sess.QueryRowCtx(ctx, &a, lockAcc, res.UserId); err != nil {
			return err
		}

		// 批次按 id 升序加锁，避免与其他事务死锁
		ids := make([]int64, 0, len(perBatch))
		for id := range perBatch {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var lines []lineDelta
		for _, id := range ids {
			var b batchRow
			const lockBatch = "SELECT `id`,`remaining`,`frozen` FROM `afv_points_batch` WHERE `id` = ? AND `user_id` = ? FOR UPDATE"
			if err := sess.QueryRowCtx(ctx, &b, lockBatch, id, res.UserId); err != nil {
				return err
			}
			amt := perBatch[id]
			if _, err := sess.ExecCtx(ctx,
				"UPDATE `afv_points_batch` SET `remaining` = `remaining` + ?, `status` = 'ACTIVE' WHERE `id` = ?", amt, id); err != nil {
				return err
			}
			lines = append(lines, lineDelta{
				BatchID: id, Amount: amt,
				BeforeRemaining: b.Remaining, AfterRemaining: b.Remaining + amt,
				BeforeFrozen: b.Frozen, AfterFrozen: b.Frozen,
			})
		}

		{
			const updAcc = "UPDATE `afv_points_account` SET `balance` = `balance` + ?, `total_consumed` = `total_consumed` - ?, " +
				"`total_refunded` = `total_refunded` + ?, `version` = `version` + 1 WHERE `id` = ?"
			if _, err := sess.ExecCtx(ctx, updAcc, total, total, total, a.Id); err != nil {
				return err
			}
			id, err := writeLog(ctx, sess, logEntry{
				UserID: res.UserId, ChangeType: changeRefund, Amount: total,
				BalanceAfter: a.Balance + total, FrozenAfter: a.FrozenBalance,
				BizType: bizTypeLocalLife, BizID: res.BizId, IdempotencyKey: "post-refund:" + no,
				Description: "人工处理退回：" + reason,
			})
			if err != nil {
				return err
			}
			if err := writeLogLines(ctx, sess, id, res.UserId, lines); err != nil {
				return err
			}
		}
		refunded = total

		const updRes = "UPDATE `afv_billing_reservation` SET `status` = ?, `refunded_time` = NOW() WHERE `id` = ? AND `status` = ?"
		ret, err := sess.ExecCtx(ctx, updRes, statusRefunded, res.Id, res.Status)
		if err != nil {
			return err
		}
		if n, _ := ret.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s 并发下已被改状态", ErrGone, no)
		}
		_, err = sess.ExecCtx(ctx, "UPDATE `afv_billing_reservation_line` SET `status` = ? WHERE `reservation_no` = ?", statusRefunded, no)
		return err
	})
	return refunded, err
}
