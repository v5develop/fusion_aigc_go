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
// 退回仍可用的原批次（remaining 加回、置 ACTIVE、expire_time 不变），原批次已过期 / 撤回的部分落 REFUND 新批次（永不过期）；
// 账户：balance 加回，total_consumed 减掉，total_refunded 加上；流水记一条 REFUND（幂等键 post-refund:{no}）。
//
// 按实际用量补退过（actual-cost-refund:{no}）的部分不再重复退：只退「消费 − 已补退」。
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
		// 按实际用量补退过（RefundOvercharge，单子仍是 PARTIALLY_REFUNDED）的部分已经回到这些批次，不能再退一遍：
		// 按补退流水明细逐批次扣掉
		var already []struct {
			BatchID int64 `db:"batch_id"`
			Amount  int64 `db:"amount"`
		}
		const qa = "SELECT ll.`batch_id`, ll.`amount` FROM `afv_points_log` pl " +
			"JOIN `afv_points_log_line` ll ON ll.`ledger_id` = pl.`id` " +
			"WHERE pl.`user_id` = ? AND pl.`idempotency_key` = ?"
		if err := sess.QueryRowsCtx(ctx, &already, qa, res.UserId, OverchargeRefundKeyPrefix+no); err != nil {
			return err
		}
		// 补退额先按批次对上（退回原批次的那部分）；落到 REFUND 新批次的那部分对不上原批次，从剩余批次里按 id 顺序扣掉，
		// 保证「本次可退 = 消费 − 已补退」
		var unmatched int64
		for _, x := range already {
			if x.Amount <= 0 {
				continue
			}
			back := min64(x.Amount, perBatch[x.BatchID])
			perBatch[x.BatchID] -= back
			total -= back
			unmatched += x.Amount - back
		}
		if unmatched > 0 {
			ids := make([]int64, 0, len(perBatch))
			for id := range perBatch {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			for _, id := range ids {
				if unmatched <= 0 {
					break
				}
				back := min64(unmatched, perBatch[id])
				if back <= 0 {
					continue
				}
				perBatch[id] -= back
				total -= back
				unmatched -= back
			}
		}
		for id, amt := range perBatch {
			if amt <= 0 {
				delete(perBatch, id)
			}
		}

		var a Account
		const lockAcc = "SELECT " + accountCols + " FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
		if err := sess.QueryRowCtx(ctx, &a, lockAcc, res.UserId); err != nil {
			return err
		}

		// 退回仍可用的原批次；原批次已过期 / 撤回的部分落 REFUND 新批次（见 restoreConsumed，与 Java 一致，不复活过期批次）
		lines, err := restoreConsumed(ctx, sess, res.UserId, a.Id, no, perBatch, total)
		if err != nil {
			return err
		}

		if total > 0 {
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
		// 明细行：整单退回，各行 refunded_points = 该行分得的点数（与 Java settleReservationLines 同口径）
		return settleLines(ctx, sess, no, res.PricePoints, res.PricePoints, statusRefunded)
	})
	return refunded, err
}
