package billing

import (
	"context"
	"math/big"
	"sort"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// settleLines 结算后把预留明细行的 confirmed_points / refunded_points / status 写成与整单一致。
//
// 照 Java BillingReservationServiceImpl.settleReservationLines：Σ行确认 = 整单实扣（total − refunded）、Σ行退款 = 整单退款；
// 各行底数 = 自己的 reserved_points（行和 ≠ 整单时按占比最大余数法把整单分到各行），退款按底数占比最大余数法分摊，
// 每行 0 ≤ 退款 ≤ 底数。Go 不做「按步骤未交付份数」的加权（我们的单子只有一个收费步骤），其余口径一致。
func settleLines(ctx context.Context, s sqlx.Session, no string, total, refunded int64, status string) error {
	var lines []struct {
		ID       int64 `db:"id"`
		Reserved int64 `db:"reserved_points"`
	}
	const q = "SELECT `id`, `reserved_points` FROM `afv_billing_reservation_line` WHERE `reservation_no` = ? ORDER BY `line_no`, `id`"
	if err := s.QueryRowsCtx(ctx, &lines, q, no); err != nil {
		return err
	}
	reserved := make([]int64, len(lines))
	for i, l := range lines {
		reserved[i] = l.Reserved
	}
	conf, ref := allocateLines(total, refunded, reserved)
	const upd = "UPDATE `afv_billing_reservation_line` SET `confirmed_points` = ?, `refunded_points` = ?, `status` = ? WHERE `id` = ?"
	for i, l := range lines {
		if _, err := s.ExecCtx(ctx, upd, conf[i], ref[i], status, l.ID); err != nil {
			return err
		}
	}
	return nil
}

// allocateLines 明细行分摊：返回 (确认[], 退款[])，与 reserved 同序。
// 保证 Σ确认 = total − refunded、Σ退款 = refunded（refunded 夹到 [0,total]）、每行 0 ≤ 退款 ≤ 该行底数。
func allocateLines(total, refunded int64, reserved []int64) (confirmed, refunds []int64) {
	n := len(reserved)
	confirmed, refunds = make([]int64, n), make([]int64, n)
	if n == 0 {
		return
	}
	var sum int64
	w := make([]int64, n)
	for i, r := range reserved {
		w[i] = max(0, r)
		sum += w[i]
	}
	base := w
	if sum != total {
		base = largestRemainder(total, w)
	}
	refunded = max(0, min(refunded, total))
	refunds = largestRemainder(refunded, base)
	var overflow int64
	for i := range refunds {
		if refunds[i] > base[i] {
			overflow += refunds[i] - base[i]
			refunds[i] = base[i]
		}
	}
	for i := 0; i < n && overflow > 0; i++ {
		moved := min(base[i]-refunds[i], overflow)
		refunds[i] += moved
		overflow -= moved
	}
	for i := range confirmed {
		confirmed[i] = base[i] - refunds[i]
	}
	return
}

// largestRemainder 把 amount 按权重分成整数份，和恒等于 amount（权重全 0 时均分，余数给前面的行）。
func largestRemainder(amount int64, weights []int64) []int64 {
	n := len(weights)
	out := make([]int64, n)
	if n == 0 || amount <= 0 {
		return out
	}
	var sum int64
	for _, x := range weights {
		sum += max(0, x)
	}
	if sum == 0 {
		for i := range out {
			out[i] = amount / int64(n)
		}
		for i := 0; i < int(amount%int64(n)); i++ {
			out[i]++
		}
		return out
	}
	type rem struct {
		i int
		r *big.Int
	}
	rems := make([]rem, n)
	var given int64
	A, S := big.NewInt(amount), big.NewInt(sum)
	for i, x := range weights {
		p := new(big.Int).Mul(A, big.NewInt(max(0, x)))
		q, m := new(big.Int).QuoRem(p, S, new(big.Int))
		out[i] = q.Int64()
		given += out[i]
		rems[i] = rem{i, m}
	}
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].r.Cmp(rems[b].r) > 0 })
	for k := 0; given < amount; k = (k + 1) % n {
		out[rems[k].i]++
		given++
	}
	return out
}
