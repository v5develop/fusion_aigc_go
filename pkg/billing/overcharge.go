package billing

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// OverchargeRefundKeyPrefix 按实际用量补退的流水幂等键前缀：actual-cost-refund:{reservationNo}。
// afv_points_log 上有 uk_idempotency_key，同一张预留单只会补退一次（重复执行、并发执行都安全）。
const OverchargeRefundKeyPrefix = "actual-cost-refund:"

// ReservationBrief 补退工具要用的预留单信息。
type ReservationBrief struct {
	No             string
	UserID         int64
	BizID          string
	Status         string
	PricePoints    int64
	VariantCode    string
	CarrierModelID int64
	// GoCreated 是 Go 冻结的单（幂等键 golife: 前缀）；补退工具只碰这种
	GoCreated bool
}

// LoadReservation 按 (用户, 请求号) 取 Go 写的预留单（任意状态）。没有返回 nil。
func (s *Service) LoadReservation(ctx context.Context, userID int64, requestID string) (*ReservationBrief, error) {
	if s == nil || s.conn == nil || requestID == "" {
		return nil, nil
	}
	var r reservationRow
	const q = "SELECT " + resCols + " FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 LIMIT 1"
	switch err := s.conn.QueryRowCtx(ctx, &r, q, bizTypeLocalLife, BizID(userID, requestID)); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	rv := toReservation(&r)
	return &ReservationBrief{No: r.No, UserID: r.UserId, BizID: r.BizId, Status: r.Status, PricePoints: r.PricePoints,
		VariantCode: rv.variantCode, CarrierModelID: rv.carrierModelID, GoCreated: strings.HasPrefix(r.IdemKey, freezePrefix)}, nil
}

// BizID 预留单 biz_id（{userId}_{requestId}，超 64 字节折叠成稳定哈希），与 Freeze 同一规则。
func BizID(userID int64, requestID string) string {
	return Charge{UserID: userID, RequestID: requestID}.bizID()
}

// CarrierOf 预留单载体的结算方式，以及按该载体的汇率、加价倍率把实报成本（美元）换成应扣点数的函数
// （补退工具用，与在线结算 SettleCost 同一口径：min(冻结, max(1, ceil(usd × 汇率 × 倍率 × 100)))）。
func (s *Service) CarrierOf(ctx context.Context, b *ReservationBrief) (mode string, fair func(usd *big.Rat) int64) {
	r := &Reservation{No: b.No, PricePoints: b.PricePoints, variantCode: b.VariantCode, carrierModelID: b.CarrierModelID}
	ci := s.carrierOfReservation(ctx, r)
	rate, markup := s.rates(ctx, ci)
	return ci.Mode, func(usd *big.Rat) int64 { return ActualPoints(b.PricePoints, usd, rate, markup) }
}

// ConsumedPoints 预留单实际消费了多少点（CONSUME 流水合计，含此前补退之前的原值）及已补退的点数。
func (s *Service) ConsumedPoints(ctx context.Context, b *ReservationBrief) (consumed, refundedBefore int64, err error) {
	var c int64
	const qc = "SELECT COALESCE(SUM(-`amount`), 0) FROM `afv_points_log` WHERE `user_id` = ? AND `change_type` = ? AND `idempotency_key` IN (?, ?)"
	if err := s.conn.QueryRowCtx(ctx, &c, qc, b.UserID, changeConsume, "confirm:"+b.No, "partial-consume:"+b.No); err != nil {
		return 0, 0, err
	}
	var rf int64
	const qr = "SELECT COALESCE(SUM(`amount`), 0) FROM `afv_points_log` WHERE `user_id` = ? AND `idempotency_key` = ?"
	if err := s.conn.QueryRowCtx(ctx, &rf, qr, b.UserID, OverchargeRefundKeyPrefix+b.No); err != nil {
		return 0, 0, err
	}
	return c, rf, nil
}

// RefundOvercharge 已确认（CONFIRMED / PARTIALLY_REFUNDED）的预留单多扣了：把它的实际消费降到 target，差额退回余额。
//
// 用于按实际用量结算上线前、按最坏上限扣费的历史单（tools/ops/actual-cost-refund）。口径与 RefundConfirmed 的退回一致：
// 退回仍可用的原批次（remaining 加回、置 ACTIVE、expire_time 不变；先退最晚过期的），原批次已过期 / 撤回的部分落 REFUND
// 新批次（永不过期，与 Java refundConfirmedExcess 一致，不复活过期批次）；
// 账户 balance 加回、total_consumed 减、total_refunded 加；流水记一条 REFUND（幂等键 actual-cost-refund:{no}）；
// 预留单置 PARTIALLY_REFUNDED，明细行按整单重新分摊 confirmed_points / refunded_points。
//
// 返回本次退回的点数。已补退过 / 消费不高于 target → 0（重复执行安全）。
func (s *Service) RefundOvercharge(ctx context.Context, reservationNo string, target int64, reason string) (int64, error) {
	if s == nil || s.conn == nil || reservationNo == "" {
		return 0, nil
	}
	if target < 0 {
		target = 0
	}
	key := OverchargeRefundKeyPrefix + reservationNo
	var refunded int64
	err := s.conn.TransactCtx(ctx, func(ctx context.Context, sess sqlx.Session) error {
		res, err := lockReservation(ctx, sess, reservationNo)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(res.IdemKey, freezePrefix) {
			// Java 的单由 Java 自己的历史补退（ActualCostHistoryRefundService）处理，两边幂等键不同，碰了会重复退
			return fmt.Errorf("billing: %s 不是 Go 冻结的单（幂等键 %s），拒绝补退", reservationNo, res.IdemKey)
		}
		switch res.Status {
		case statusConfirmed, statusPartiallyRefunded:
		default:
			return fmt.Errorf("%w: %s 当前是 %s，只补退已确认的单子", ErrGone, reservationNo, res.Status)
		}
		var exists int64
		if err := sess.QueryRowCtx(ctx, &exists, "SELECT COUNT(1) FROM `afv_points_log` WHERE `idempotency_key` = ?", key); err != nil {
			return err
		}
		if exists > 0 {
			return nil // 幂等：补退过了
		}

		var consumed []struct {
			BatchID int64 `db:"batch_id"`
			Amount  int64 `db:"amount"`
		}
		const qc = "SELECT ll.`batch_id`, -ll.`amount` AS `amount` FROM `afv_points_log` pl " +
			"JOIN `afv_points_log_line` ll ON ll.`ledger_id` = pl.`id` " +
			"WHERE pl.`user_id` = ? AND pl.`change_type` = ? AND pl.`idempotency_key` IN (?, ?)"
		if err := sess.QueryRowsCtx(ctx, &consumed, qc, res.UserId, changeConsume, "confirm:"+reservationNo, "partial-consume:"+reservationNo); err != nil {
			return err
		}
		perBatch := map[int64]int64{}
		var total int64
		for _, c := range consumed {
			if c.Amount <= 0 {
				continue
			}
			perBatch[c.BatchID] += c.Amount
			total += c.Amount
		}
		if total <= 0 {
			return fmt.Errorf("billing: %s 已确认但找不到消费流水，拒绝补退，请人工核对", reservationNo)
		}
		amount := total - target
		if amount <= 0 {
			return nil
		}

		var a Account
		const lockAcc = "SELECT " + accountCols + " FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
		if err := sess.QueryRowCtx(ctx, &a, lockAcc, res.UserId); err != nil {
			return err
		}

		// 退回仍可用的原批次（晚过期的先退）；原批次已过期 / 撤回的部分落 REFUND 新批次（见 restoreConsumed）
		lines, err := restoreConsumed(ctx, sess, res.UserId, a.Id, reservationNo, perBatch, amount)
		if err != nil {
			return err
		}

		const updAcc = "UPDATE `afv_points_account` SET `balance` = `balance` + ?, `total_consumed` = `total_consumed` - ?, " +
			"`total_refunded` = `total_refunded` + ?, `version` = `version` + 1 WHERE `id` = ?"
		if _, err := sess.ExecCtx(ctx, updAcc, amount, amount, amount, a.Id); err != nil {
			return err
		}
		id, err := writeLog(ctx, sess, logEntry{
			UserID: res.UserId, ChangeType: changeRefund, Amount: amount,
			BalanceAfter: a.Balance + amount, FrozenAfter: a.FrozenBalance,
			BizType: bizTypeLocalLife, BizID: res.BizId, IdempotencyKey: key,
			Description: trim(or(reason, "按实际用量补退多扣点数"), 512),
		})
		if err != nil {
			if isDup(err) {
				return errDupOvercharge
			}
			return err
		}
		if err := writeLogLines(ctx, sess, id, res.UserId, lines); err != nil {
			return err
		}
		const updRes = "UPDATE `afv_billing_reservation` SET `status` = ?, `refunded_time` = NOW() WHERE `id` = ? AND `status` = ?"
		ret, err := sess.ExecCtx(ctx, updRes, statusPartiallyRefunded, res.Id, res.Status)
		if err != nil {
			return err
		}
		if n, _ := ret.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s 并发下已被改状态", ErrGone, reservationNo)
		}
		// 明细行按整单重新分摊：确认 = 消费 − 本次补退，其余都算退款
		confirmedAfter := total - amount
		if err := settleLines(ctx, sess, reservationNo, res.PricePoints, max(0, res.PricePoints-confirmedAfter), statusPartiallyRefunded); err != nil {
			return err
		}
		refunded = amount
		return nil
	})
	if errors.Is(err, errDupOvercharge) {
		return 0, nil // 并发下别人先补退了
	}
	return refunded, err
}

var errDupOvercharge = errors.New("billing: 补退幂等键已存在")

// ListSettledGoReservations Go 写的（golife: 幂等键）、已确认（CONFIRMED / PARTIALLY_REFUNDED）的预留单，按创建时间窗口。
// 补退工具用来找「对不上当前 run / 子任务」的单（如成功后又重新生成、请求号被新一轮覆盖的旧轮次），只报告、不自动退。
func (s *Service) ListSettledGoReservations(ctx context.Context, from, to time.Time, limit int) ([]*ReservationBrief, error) {
	var rows []reservationRow
	q := "SELECT " + resCols + " FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `idempotency_key` LIKE ? " +
		"AND `status` IN (?, ?) AND `create_time` >= ? AND `create_time` < ? AND `deleted` = 0 ORDER BY `id` LIMIT ?"
	if err := s.conn.QueryRowsCtx(ctx, &rows, q, bizTypeLocalLife, freezePrefix+"%", statusConfirmed, statusPartiallyRefunded, from, to, limit); err != nil {
		return nil, err
	}
	out := make([]*ReservationBrief, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		rv := toReservation(r)
		out = append(out, &ReservationBrief{No: r.No, UserID: r.UserId, BizID: r.BizId, Status: r.Status, PricePoints: r.PricePoints,
			VariantCode: rv.variantCode, CarrierModelID: rv.carrierModelID, GoCreated: strings.HasPrefix(r.IdemKey, freezePrefix)})
	}
	return out, nil
}
