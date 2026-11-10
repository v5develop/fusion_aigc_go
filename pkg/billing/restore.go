package billing

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// batchSourceRefund 退不回原批次时新建批次的来源（Java PointsBatchSourceEnum.REFUND）。
const batchSourceRefund = "REFUND"

// restoreConsumed 把已确认消费的点数退回（照 Java BillingReservationServiceImpl.restoreConsumedBatches）：
//   - 只退回仍可用的原批次（非 EXPIRED / REVOKED、未到期），长效（无到期 / 到期晚）的先退，每批最多退它被本单消费的额；
//     原批次 remaining 加回并置 ACTIVE，expire_time 不变；
//   - 退不回去的部分（原批次已过期 / 撤回）落一个 source=REFUND、source_id=预留单号、永不过期（expire_time NULL，
//     与 Java 一致）的新批次，用户不因批次过期吃亏，也不会把过期批次「复活」后又被过期任务收走。
//
// perBatch：本单在各批次上的可退额（批次 id → 点数）。返回给退款流水写的批次明细（和 = amount）。
// 调用方须已锁住账户；批次在这里按 id 升序加锁（避免死锁）。
func restoreConsumed(ctx context.Context, s sqlx.Session, userID, accountID int64, no string, perBatch map[int64]int64, amount int64) ([]lineDelta, error) {
	if amount <= 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(perBatch))
	for id, amt := range perBatch {
		if amt > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	type lockedBatch struct {
		batchRow
		Expire sql.NullTime `db:"expire_time"`
		Live   int64        `db:"live"`
	}
	bs := make([]lockedBatch, 0, len(ids))
	for _, id := range ids {
		var b lockedBatch
		const lockBatch = "SELECT `id`,`remaining`,`frozen`,`expire_time`, " +
			"(`status` NOT IN ('EXPIRED','REVOKED') AND (`expire_time` IS NULL OR `expire_time` > NOW())) AS `live` " +
			"FROM `afv_points_batch` WHERE `id` = ? AND `user_id` = ? FOR UPDATE"
		switch err := s.QueryRowCtx(ctx, &b, lockBatch, id, userID); {
		case errors.Is(err, sqlx.ErrNotFound):
			continue // 原批次没了：这部分落新批次
		case err != nil:
			return nil, err
		}
		bs = append(bs, b)
	}
	sort.SliceStable(bs, func(i, j int) bool {
		ei, ej := bs[i].Expire, bs[j].Expire
		switch {
		case !ei.Valid && !ej.Valid:
			return bs[i].Id > bs[j].Id
		case !ei.Valid: // 不过期的最晚，先退
			return true
		case !ej.Valid:
			return false
		case !ei.Time.Equal(ej.Time):
			return ei.Time.After(ej.Time)
		}
		return bs[i].Id > bs[j].Id
	})
	left := amount
	var lines []lineDelta
	for _, b := range bs {
		if left <= 0 {
			break
		}
		if b.Live == 0 {
			continue
		}
		amt := min64(perBatch[b.Id], left)
		if amt <= 0 {
			continue
		}
		if _, err := s.ExecCtx(ctx,
			"UPDATE `afv_points_batch` SET `remaining` = `remaining` + ?, `status` = 'ACTIVE' WHERE `id` = ?", amt, b.Id); err != nil {
			return nil, err
		}
		lines = append(lines, lineDelta{
			BatchID: b.Id, Amount: amt,
			BeforeRemaining: b.Remaining, AfterRemaining: b.Remaining + amt,
			BeforeFrozen: b.Frozen, AfterFrozen: b.Frozen,
		})
		left -= amt
	}
	if left > 0 {
		const ins = "INSERT INTO `afv_points_batch` (`account_id`,`user_id`,`amount`,`remaining`,`frozen`,`source`,`source_id`,`expire_time`,`status`) " +
			"VALUES (?,?,?,?,0,?,?,NULL,'ACTIVE')"
		ret, err := s.ExecCtx(ctx, ins, accountID, userID, left, left, batchSourceRefund, trim(no, 64))
		if err != nil {
			return nil, err
		}
		id, err := ret.LastInsertId()
		if err != nil {
			return nil, err
		}
		lines = append(lines, lineDelta{BatchID: id, Amount: left, BeforeRemaining: 0, AfterRemaining: left})
		logx.WithContext(ctx).Infow("billing: 退款原批次已过期 / 撤回，落新批次 判据=ORIGINAL_BATCH_UNUSABLE",
			logx.Field("check", "ORIGINAL_BATCH_UNUSABLE"), logx.Field("reservation_no", no),
			logx.Field("user_id", userID), logx.Field("batch_id", id), logx.Field("points", left))
	}
	return lines, nil
}
