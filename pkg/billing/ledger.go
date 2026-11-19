package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 编号与幂等键格式——**照抄库里的真实数据，别自创**：
//
//	reservation_no  BR + 32 hex   BRaf69b54b3fb84297b720a8493d239db5
//	ledger_no       PL + 32 hex   PLbba57b68e6a7439586447866fb07cab3
//	biz_type        CREATION
//	biz_id          {userId}_{requestId}
//
// 幂等键：
//
//	FREEZE   Java 用过 doubao: / user:N:creation: / ecommerce-group: 三种前缀。
//	         我们用 golife:{bizId} —— 多一种不冲突，而且运维一眼能看出哪些单是 Go 写的，
//	         回收器也靠这个前缀只收自己的（见 reclaim.go）。
//	CONSUME  confirm:{reservationNo}    ← 和 Java 一致
//	REFUND   refunded:{reservationNo}   ← 和 Java 一致
const (
	// bizTypeLocalLife 我们的业务类型。**这个值必须是 Java 不认识的**。
	//
	// 它的回收器（BillingHeldReservationRecoveryServiceImpl，每 60 秒一轮）
	// 扫全库所有过期 HELD 单，**不按幂等键或来源过滤**，扫出来后按 biz_type 分派：
	// IMAGE_GEN / VIDEO_GEN / CREATION 会去查它自己的任务表、自动确认或退款；
	// 认不出来的只打一条 WARN 就跳过。
	//
	// 所以用 CREATION 会被它接管——而我们的 biz_id 在它的任务表里根本不存在。
	// 用自己的值，它就不会碰。2026-10-10 Java 回复：回收查询已改成只选它自己枚举里的 biz_type
	// （IMAGE_GEN VIDEO_GEN TEXT_CHAT AI_TOOL CREATION COMPAT_SYNC AGENT_TURN），补偿接口也拒绝非 Java 的单，
	// 所以我们的单不会再占它的名额、也不会被它误处理。
	bizTypeLocalLife = "LOCALLIFE"
	freezePrefix     = "golife:"

	statusHeld              = "HELD"
	statusConfirmed         = "CONFIRMED"
	statusRefunded          = "REFUNDED"
	statusPartiallyRefunded = "PARTIALLY_REFUNDED" // 部分结算的终态，照 Java

	changeFreeze   = "FREEZE"
	changeConsume  = "CONSUME"
	changeRefund   = "REFUND"
	changeUnfreeze = "UNFREEZE" // 预留过期/取消走这个，不是 REFUND
)

func newNo(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Account 点数账户。
type Account struct {
	Id            int64 `db:"id"`
	UserId        int64 `db:"user_id"`
	Balance       int64 `db:"balance"`
	FrozenBalance int64 `db:"frozen_balance"`
	Version       int64 `db:"version"`
}

const accountCols = "`id`,`user_id`,`balance`,`frozen_balance`,`version`"

// loadAccount 读账户。没有账户返回零值——新用户开箱就是没有的，不是异常。
func loadAccount(ctx context.Context, conn sqlx.SqlConn, userID int64) (*Account, error) {
	var a Account
	const q = "SELECT " + accountCols + " FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &a, q, userID); {
	case errors.Is(err, sqlx.ErrNotFound):
		return &Account{UserId: userID}, nil
	case err != nil:
		return nil, err
	}
	return &a, nil
}

// batchRow 一个点数批次。
type batchRow struct {
	Id        int64 `db:"id"`
	Remaining int64 `db:"remaining"`
	Frozen    int64 `db:"frozen"`
}

// isDup 唯一键冲突。
//
// 幂等就靠它：不先查后写（那样两个并发请求会双双查空、双双插入），
// 直接插，撞了 1062 说明别人已经写过，去把那条读回来。
// afv_billing_reservation 上 uk_biz(biz_type,biz_id) 和 uk_idempotency_key 两把唯一键都在。
func isDup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// freeze 冻结：余额 → 冻结。
//
// **加锁顺序必须和 Java 的 doReserve 一字不差**：幂等键行 → 账户 → 批次。
// 两个服务对同一批行以不同顺序加锁就会死锁，这不是概率问题，是迟早的事。
// 所以这里宁可写得啰嗦一点（先锁再查再锁，最后才插预留单），也要跟它对齐：
//
//  1. SELECT ... FOR UPDATE by idempotency_key —— 命中即返回原单（幂等）
//  2. SELECT ... FOR UPDATE 账户
//  3. 再查一次幂等键（锁账户期间可能有人插了）
//  4. 余额校验
//  5. 改账户：balance -= P, frozen += P
//  6. 写 FREEZE 流水
//  7. 扣批次（FOR UPDATE）
//  8. **最后**插预留单
//  9. 插明细行
//
// 和 Java 唯一的差别：它第 8 步撞唯一键时手写一条 UNFREEZE 补偿，
// 我们整个事务回滚，更简单也更不容易漏。
func freeze(ctx context.Context, conn sqlx.SqlConn, c Charge, q *Quote, hold time.Duration) (*Reservation, error) {
	bizID := c.bizID()
	idem := freezePrefix + bizID
	no := newNo("BR")
	p := q.PricePoints
	if p <= 0 {
		// 算出来 0 点（比如免费档）：不写账，也不用确认/退回
		return nil, nil
	}

	var out *Reservation
	err := retryDeadlock(ctx, func() error {
		out = nil
		return freezeTx(ctx, conn, c, q, hold, bizID, idem, no, p, &out)
	})

	switch {
	case err == nil:
		return out, nil
	case isDup(err):
		// 并发下同一个 bizId 有人先写成功了，读回来原样返回，不重复扣
		logx.WithContext(ctx).Infof("billing: %s 并发冻结，返回已有的单", bizID)
		r, lerr := loadHeldOrAny(ctx, conn, bizID)
		if r != nil {
			r.replayed = true
		}
		return r, lerr
	default:
		return nil, err
	}
}

// deadlockRetries 冻结事务遇 InnoDB 死锁（1213）时最多重做几次。
//
// 同一用户并发冻结（如 studio 一次提交多张并发冻结、或多个请求同时提交）时：各事务先对不存在的幂等键 FOR UPDATE，
// 在唯一索引的同一间隙上各留一把间隙锁（互相兼容），先拿到账户锁的事务插预留单时要等别人的间隙锁、别人又在等它的账户锁，
// InnoDB 判死锁回滚其中一个。被回滚的事务什么都没写（整笔回滚），按同一幂等键重做是安全的：
// 对方已提交则第 1 步命中原单原样返回，否则重新排队拿锁，不会重复冻结。
// 4 张并发冻结时最不走运的一张最多可能连当 3 次牺牲者，留些余量。
const deadlockRetries = 5

// deadlockBackoff 第 i 次重做前的退避：10ms×(i+1) 再加同样幅度的随机抖动。
// 不加抖动时互相死锁的几笔事务会同一时刻重做、再撞一次。
func deadlockBackoff(i int) time.Duration {
	base := time.Duration(10*(i+1)) * time.Millisecond
	return base + mrand.N(base)
}

// retryDeadlock 执行 fn，遇死锁回滚（1213）时短暂退避后重做，其余错误原样返回。
func retryDeadlock(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; ; i++ {
		if err = fn(); err == nil || !isDeadlock(err) || i >= deadlockRetries {
			return err
		}
		logx.WithContext(ctx).Infof("billing: 冻结事务死锁回滚，第 %d 次重做: %v", i+1, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(deadlockBackoff(i)):
		}
	}
}

// isDeadlock MySQL 死锁回滚（ER_LOCK_DEADLOCK 1213）。
func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

// freezeTx 冻结的一次事务（见 freeze 的加锁顺序说明）。
func freezeTx(ctx context.Context, conn sqlx.SqlConn, c Charge, q *Quote, hold time.Duration,
	bizID, idem, no string, p int64, outp **Reservation) error {
	return conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		out := (*Reservation)(nil)
		defer func() { *outp = out }()
		// 1) 先锁幂等键行。已经有了说明是重试，原样返回。
		if old, err := lockByIdem(ctx, s, idem); err != nil {
			return err
		} else if old != nil {
			out = toReservation(old)
			out.replayed = true
			return nil
		}

		// 2) 锁账户
		a, err := lockAccount(ctx, s, c.UserID)
		if err != nil {
			return err
		}

		// 3) 锁账户这段时间里别人可能插进来了，再查一次
		if old, err := lockByIdem(ctx, s, idem); err != nil {
			return err
		} else if old != nil {
			out = toReservation(old)
			out.replayed = true
			return nil
		}

		// 4) 余额
		if a.Balance < p {
			return &NotEnough{Need: p, Available: a.Balance}
		}

		// 5) 账户：余额 → 冻结
		const updAcc = "UPDATE `afv_points_account` SET `balance` = `balance` - ?, " +
			"`frozen_balance` = `frozen_balance` + ?, `version` = `version` + 1 WHERE `id` = ?"
		if _, err := s.ExecCtx(ctx, updAcc, p, p, a.Id); err != nil {
			return err
		}

		// 6) 账本：FREEZE 记 -P，balance_after 是扣后的，frozen_after 是加后的。
		//    operator 照 Java 记 USER/{userId}（确认和退回才是 SYSTEM）。
		ledgerID, err := writeLog(ctx, s, logEntry{
			UserID: c.UserID, ChangeType: changeFreeze, Amount: -p,
			BalanceAfter: a.Balance - p, FrozenAfter: a.FrozenBalance + p,
			BizType: bizTypeLocalLife, BizID: bizID, IdempotencyKey: idem,
			OperatorType: "USER", OperatorID: sql.NullString{String: strconv.FormatInt(c.UserID, 10), Valid: true},
			Description: fmt.Sprintf("%s 生成冻结", q.EntryCode),
		})
		if err != nil {
			return err
		}

		// 7) 批次 FIFO
		lines, err := takeFromBatches(ctx, s, c.UserID, p)
		if err != nil {
			return err
		}
		if err := writeLogLines(ctx, s, ledgerID, c.UserID, lines); err != nil {
			return err
		}

		// 8) 最后插预留单。撞唯一键 = 并发下别人先插了，整笔回滚重来。
		const insRes = "INSERT INTO `afv_billing_reservation` " +
			"(`reservation_no`,`user_id`,`feature_code`,`model_id`,`pricing_id`,`price_points`,`price_snapshot`," +
			"`biz_type`,`biz_id`,`status`,`idempotency_key`,`expire_time`) " +
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"
		if _, err := s.ExecCtx(ctx, insRes, no, c.UserID, q.FeatureCode, nullInt(q.ModelID), nullInt(q.PricingID),
			p, q.Snapshot(), bizTypeLocalLife, bizID, statusHeld, idem, time.Now().Add(hold)); err != nil {
			return err
		}

		// 9) 明细行：一个计费步骤一行
		if err := writeResLines(ctx, s, no, q); err != nil {
			return err
		}
		out = &Reservation{No: no, PricePoints: p, Quote: q, variantCode: q.VariantCode, carrierModelID: q.CarrierModelID}
		return nil
	})
}

// lockByIdem 按幂等键锁住预留单。没有就返回 nil——
// 对不存在的行 FOR UPDATE 会在唯一索引上留间隙锁，这正是并发下互斥的关键，
// 也是必须和 Java 用同一个顺序加锁的原因。
func lockByIdem(ctx context.Context, s sqlx.Session, idem string) (*reservationRow, error) {
	var r reservationRow
	const q = "SELECT " + resCols + " FROM `afv_billing_reservation` " +
		"WHERE `idempotency_key` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
	switch err := s.QueryRowCtx(ctx, &r, q, idem); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &r, nil
}

// lockAccount 锁账户。没有账户就先建再锁——
// 新用户开箱是没有账户的，这不是异常。并发建账靠 uk_user_id 挡。
func lockAccount(ctx context.Context, s sqlx.Session, userID int64) (*Account, error) {
	const q = "SELECT " + accountCols + " FROM `afv_points_account` " +
		"WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
	var a Account
	err := s.QueryRowCtx(ctx, &a, q, userID)
	if err == nil {
		return &a, nil
	}
	if !errors.Is(err, sqlx.ErrNotFound) {
		return nil, err
	}
	const ins = "INSERT INTO `afv_points_account` (`user_id`) VALUES (?)"
	if _, err := s.ExecCtx(ctx, ins, userID); err != nil && !isDup(err) {
		return nil, err
	}
	if err := s.QueryRowCtx(ctx, &a, q, userID); err != nil {
		return nil, err
	}
	return &a, nil
}

// takeFromBatches 从批次里冻结 total 点，返回每个批次动了多少。
//
// 顺序和 Java 一致：先过期的先用（expire_time 升序，NULL 排最后），同期按 id。
// 固定顺序同时也避免了和它并发时的死锁。
func takeFromBatches(ctx context.Context, s sqlx.Session, userID, total int64) ([]lineDelta, error) {
	var rows []batchRow
	// 排序和可消费条件都照 Java（PointsBatchMapper.selectConsumableBatchesForUpdate）：
	//   ORDER BY expire_time IS NULL, expire_time ASC, create_time ASC, id ASC
	// ⚠️ **不能加 expire_time > NOW()**。它的判据只有 status='ACTIVE'，
	// 批次过期靠另一个 5 分钟一轮的定时任务去清。在"已过期但还没被扫走"的窗口里，
	// 那些点数仍然算在 account.balance 里、也仍然会被消费。我们要是把它们排除掉，
	// 就会出现"账户余额够、批次凑不齐"而整笔失败。
	const q = "SELECT `id`,`remaining`,`frozen` FROM `afv_points_batch` " +
		"WHERE `user_id` = ? AND `status` = 'ACTIVE' AND `deleted` = 0 AND `remaining` > 0 " +
		"ORDER BY (`expire_time` IS NULL), `expire_time`, `create_time`, `id` FOR UPDATE"
	if err := s.QueryRowsCtx(ctx, &rows, q, userID); err != nil {
		return nil, err
	}
	var out []lineDelta
	left := total
	for _, b := range rows {
		if left <= 0 {
			break
		}
		take := min64(b.Remaining, left)
		const upd = "UPDATE `afv_points_batch` SET `remaining` = `remaining` - ?, `frozen` = `frozen` + ? WHERE `id` = ?"
		if _, err := s.ExecCtx(ctx, upd, take, take, b.Id); err != nil {
			return nil, err
		}
		out = append(out, lineDelta{
			BatchID: b.Id, Amount: -take,
			BeforeRemaining: b.Remaining, AfterRemaining: b.Remaining - take,
			BeforeFrozen: b.Frozen, AfterFrozen: b.Frozen + take,
		})
		left -= take
	}
	if left > 0 {
		// 账户余额够但批次凑不齐：说明账户和批次已经不一致了。
		// 宁可整笔回滚报错，也不要扣一半——不一致要人来查，不能让它继续扩散。
		return nil, fmt.Errorf("billing: 用户 %d 批次余额凑不齐 %d（还差 %d），账户与批次不一致", userID, total, left)
	}
	return out, nil
}

// settle 确认：钱真正离开。actual 是实际产出量，差额同一事务退回。
//
// 账务细节全部对齐 Java 的 doPartialConfirm（BillingReservationServiceImpl:190-246）：
//   - 终态是 PARTIALLY_REFUNDED，不是 CONFIRMED
//   - 两把幂等键是 partial-consume:{no} / partial-refund:{no}
//   - **两条流水各记自己那一步之后的账面**：CONSUME 写 balanceBefore（余额那时还没变）
//     和 frozenBefore-charged；REFUND 才写最终值。
//     它代码里的注释说明了原因：按流水顺序重放对账时，balance_after 与 amount 累加要对得上。
//   - charged 全额或 0 时退化成整单确认 / 整单退，不写 PARTIALLY_REFUNDED
func settle(ctx context.Context, conn sqlx.SqlConn, r *Reservation, actual int) error {
	_, err := settleWith(ctx, conn, r, func(total int64) int64 {
		consume, _ := splitActual(r, total, actual)
		return consume
	}, "按实际交付量确认消费", "未交付部分退回")
	return err
}

// settlePoints 按点数确认（部分确认）：确认 points（夹到 [0, 冻结]），差额同一事务退回。
// 返回 done=false 表示单子此前已结算（幂等命中，没有再动账）。
func settlePoints(ctx context.Context, conn sqlx.SqlConn, r *Reservation, points int64, consumeDesc, refundDesc string) (bool, error) {
	return settleWith(ctx, conn, r, func(total int64) int64 { return points }, consumeDesc, refundDesc)
}

// settleWith 结算的公共部分：consumeOf(冻结点数) 给出确认多少，其余退回。
func settleWith(ctx context.Context, conn sqlx.SqlConn, r *Reservation, consumeOf func(total int64) int64, consumeDesc, refundDesc string) (bool, error) {
	done := false
	err := conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		res, err := lockReservation(ctx, s, r.No)
		if err != nil {
			return err
		}
		if res.Status != statusHeld {
			// 已经结算过就当成功（幂等）；被别人回收了要报警。
			if res.Status == statusConfirmed || res.Status == statusPartiallyRefunded {
				return nil
			}
			return fmt.Errorf("%w: %s 当前是 %s", ErrGone, r.No, res.Status)
		}

		total := res.PricePoints
		consume := consumeOf(total)
		if consume < 0 {
			consume = 0
		}
		if consume > total {
			consume = total
		}
		back := total - consume

		var a Account
		const lockAcc = "SELECT " + accountCols + " FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
		if err := s.QueryRowCtx(ctx, &a, lockAcc, res.UserId); err != nil {
			return err
		}

		// 批次：确认的部分 frozen 直接减掉（钱消失，不回 remaining）；
		//       差额部分 frozen 减、remaining 加（退回原批次，过期时间不变）
		lines, err := releaseFrozen(ctx, s, res, consume, back)
		if err != nil {
			return err
		}

		const updAcc = "UPDATE `afv_points_account` SET `frozen_balance` = `frozen_balance` - ?, " +
			"`balance` = `balance` + ?, `total_consumed` = `total_consumed` + ?, " +
			"`total_refunded` = `total_refunded` + ?, `version` = `version` + 1 WHERE `id` = ?"
		if _, err := s.ExecCtx(ctx, updAcc, total, back, consume, back, a.Id); err != nil {
			return err
		}

		status, consumeKey, refundKey := statusConfirmed, "confirm:"+r.No, "refunded:"+r.No
		if back > 0 {
			status, consumeKey, refundKey = statusPartiallyRefunded, "partial-consume:"+r.No, "partial-refund:"+r.No
		}

		// CONSUME：balance 不动（钱在冻结那步就出余额了），frozen 只减掉确认的部分
		ledgerID, err := writeLog(ctx, s, logEntry{
			UserID: res.UserId, ChangeType: changeConsume, Amount: -consume,
			BalanceAfter: a.Balance, FrozenAfter: a.FrozenBalance - consume,
			BizType: bizTypeLocalLife, BizID: res.BizId, IdempotencyKey: consumeKey,
			Description: consumeDesc,
		})
		if err != nil {
			return err
		}
		if err := writeLogLines(ctx, s, ledgerID, res.UserId, lines.consumed); err != nil {
			return err
		}
		if back > 0 {
			// 差额退回，记最终账面
			id, err := writeLog(ctx, s, logEntry{
				UserID: res.UserId, ChangeType: changeRefund, Amount: back,
				BalanceAfter: a.Balance + back, FrozenAfter: a.FrozenBalance - total,
				BizType: bizTypeLocalLife, BizID: res.BizId, IdempotencyKey: refundKey,
				Description: refundDesc,
			})
			if err != nil {
				return err
			}
			if err := writeLogLines(ctx, s, id, res.UserId, lines.returned); err != nil {
				return err
			}
		}

		const updRes = "UPDATE `afv_billing_reservation` SET `status` = ?, `confirmed_time` = NOW()" +
			", `refunded_time` = IF(? > 0, NOW(), `refunded_time`) WHERE `id` = ? AND `status` = ?"
		ret, err := s.ExecCtx(ctx, updRes, status, back, res.Id, statusHeld)
		if err != nil {
			return err
		}
		if n, _ := ret.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s 并发下已被改状态", ErrGone, r.No)
		}
		// 明细行：状态跟主单走（部分结算 PARTIALLY_REFUNDED，否则 CONFIRMED），并按 Java settleReservationLines 的口径
		// 回填 confirmed_points / refunded_points（Σ行确认 = 整单实扣、Σ行退款 = 整单退款，最大余数法分摊）。
		// Java 的 V1.0.166 已把状态列加宽到 32（2026-10-10 Java 回复确认），不用再绕。
		if err := settleLines(ctx, s, r.No, total, back, status); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}

// splitActual 按实际产出量算确认多少、退多少。
//
// ⚠️ 只有 settlement_type=PARTIAL 的档位才允许部分结算。ALL_OR_NOTHING 的
// 一律全额确认——这是 Java 的配置语义，我们不能自己放宽，否则两边对不上账。
func splitActual(r *Reservation, reserved int64, actual int) (consume, back int64) {
	if actual <= 0 || r.Quote == nil || r.Quote.Settlement != "PARTIAL" || r.Quote.Quantity <= 0 {
		return reserved, 0
	}
	if actual >= r.Quote.Quantity {
		return reserved, 0
	}
	consume = reserved * int64(actual) / int64(r.Quote.Quantity)
	return consume, reserved - consume
}

// refund 退回：冻结原路回到余额。
//
// change 与幂等键照 Java 分两种：
//   - 业务失败 → REFUND / refunded:{no} → 状态 REFUNDED
//   - 预留过期或取消 → UNFREEZE / expired:{no}（或 cancelled:）→ 状态 EXPIRED
//
// 之前我把过期也写成 REFUND 了，对不上它的账目分类。
func refund(ctx context.Context, conn sqlx.SqlConn, no, reason string) error {
	return unfreeze(ctx, conn, no, changeRefund, "refunded:"+no, statusRefunded, or(reason, "生成失败退回"))
}

// errLeaseRenewed 回收时发现租约在扫描之后被续期，本条不该回收。
var errLeaseRenewed = errors.New("billing: 租约已被续期")

// release 预留过期回收。
//
// 只有「锁住行之后仍然过期」才回收：扫描与逐条回收之间 Renew 可能已把租约顺延（Renew 与这里的 FOR UPDATE 互斥，
// 谁先拿到行锁谁先生效），此时返回 errLeaseRenewed，调用方跳过，不退点也不置 EXPIRED。
func release(ctx context.Context, conn sqlx.SqlConn, no string) error {
	return unfreeze(ctx, conn, no, changeUnfreeze, "expired:"+no, "EXPIRED", "预留超时自动释放")
}

func unfreeze(ctx context.Context, conn sqlx.SqlConn, no, change, idem, target, reason string) error {
	return conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		res, err := lockReservation(ctx, s, no)
		if err != nil {
			return err
		}
		if res.Status != statusHeld {
			if res.Status == target {
				return nil // 幂等
			}
			return fmt.Errorf("%w: %s 当前是 %s", ErrGone, no, res.Status)
		}
		if target == "EXPIRED" {
			// 行锁下复核（锁定读，读最新已提交值）：扫描之后租约若被 Renew 顺延，就不是真过期，跳过
			var expired int64
			const chk = "SELECT COUNT(1) FROM `afv_billing_reservation` WHERE `id` = ? AND `expire_time` IS NOT NULL AND `expire_time` < NOW() FOR UPDATE"
			if err := s.QueryRowCtx(ctx, &expired, chk, res.Id); err != nil {
				return err
			}
			if expired == 0 {
				return errLeaseRenewed
			}
		}
		p := res.PricePoints

		var a Account
		const lockAcc = "SELECT " + accountCols + " FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
		if err := s.QueryRowCtx(ctx, &a, lockAcc, res.UserId); err != nil {
			return err
		}
		lines, err := releaseFrozen(ctx, s, res, 0, p)
		if err != nil {
			return err
		}
		const updAcc = "UPDATE `afv_points_account` SET `frozen_balance` = `frozen_balance` - ?, " +
			"`balance` = `balance` + ?, `total_refunded` = `total_refunded` + ?, `version` = `version` + 1 WHERE `id` = ?"
		if _, err := s.ExecCtx(ctx, updAcc, p, p, p, a.Id); err != nil {
			return err
		}
		ledgerID, err := writeLog(ctx, s, logEntry{
			UserID: res.UserId, ChangeType: change, Amount: p,
			BalanceAfter: a.Balance + p, FrozenAfter: a.FrozenBalance - p,
			BizType: bizTypeLocalLife, BizID: res.BizId, IdempotencyKey: idem,
			Description: trim(reason, 512),
		})
		if err != nil {
			return err
		}
		if err := writeLogLines(ctx, s, ledgerID, res.UserId, lines.returned); err != nil {
			return err
		}
		const updRes = "UPDATE `afv_billing_reservation` SET `status` = ?, `refunded_time` = NOW() " +
			"WHERE `id` = ? AND `status` = ?"
		ret, err := s.ExecCtx(ctx, updRes, target, res.Id, statusHeld)
		if err != nil {
			return err
		}
		if n, _ := ret.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s 并发下已被改状态", ErrGone, no)
		}
		// 明细行：整单退回，各行 refunded_points = 该行分得的点数（与 Java settleReservationLines 同口径）
		return settleLines(ctx, s, no, p, p, target)
	})
}

type splitLines struct{ consumed, returned []lineDelta }

// releaseFrozen 解冻：consume 部分钱消失，back 部分回到 remaining。
//
// 只动**这张预留单自己冻结的批次**：按它的 FREEZE 流水明细（幂等键 = 预留单幂等键）找回批次和数量，
// 不能凭「哪个批次 frozen > 0」去猜——同一用户同时有 Java 的单也冻在这些批次上，猜会拿错。
// 口径照 Java（BillingReservationServiceImpl:335-361、744-746）：
//   - 按批次 expire_time 升序（空值最晚）：先确认的从最早过期的批次扣，差额退回后面的
//   - 退回原批次，并把批次置回 ACTIVE——即使它已经 EXPIRED（之后由过期任务再过期一次）
//   - 扣完（remaining 和 frozen 都为 0）的批次置 EXHAUSTED
func releaseFrozen(ctx context.Context, s sqlx.Session, res *reservationRow, consume, back int64) (*splitLines, error) {
	var freezes []struct {
		BatchID int64 `db:"batch_id"`
		Amount  int64 `db:"amount"`
	}
	const ql = "SELECT ll.`batch_id`, -ll.`amount` AS `amount` FROM `afv_points_log` pl " +
		"JOIN `afv_points_log_line` ll ON ll.`ledger_id` = pl.`id` " +
		"WHERE pl.`idempotency_key` = ? AND pl.`change_type` = 'FREEZE' AND pl.`user_id` = ?"
	if err := s.QueryRowsCtx(ctx, &freezes, ql, res.IdemKey, res.UserId); err != nil {
		return nil, err
	}
	if len(freezes) == 0 {
		return nil, fmt.Errorf("billing: %s 的冻结流水明细不存在（幂等键 %s）", res.No, res.IdemKey)
	}
	own := make(map[int64]int64, len(freezes))
	ids := make([]any, 0, len(freezes))
	ph := make([]string, 0, len(freezes))
	for _, f := range freezes {
		own[f.BatchID] += f.Amount
		ids = append(ids, f.BatchID)
		ph = append(ph, "?")
	}
	var rows []batchRow
	q := "SELECT `id`,`remaining`,`frozen` FROM `afv_points_batch` WHERE `id` IN (" + strings.Join(ph, ",") + ") " +
		"AND `user_id` = ? ORDER BY (`expire_time` IS NULL), `expire_time`, `create_time`, `id` FOR UPDATE"
	if err := s.QueryRowsCtx(ctx, &rows, q, append(ids, res.UserId)...); err != nil {
		return nil, err
	}
	out := &splitLines{}
	leftC, leftB := consume, back
	for _, b := range rows {
		if leftC <= 0 && leftB <= 0 {
			break
		}
		frozen := min64(b.Frozen, own[b.Id])
		touchedBack := false
		// 先扣确认的，再退差额——和冻结时的批次顺序一致
		if leftC > 0 && frozen > 0 {
			take := min64(frozen, leftC)
			const upd = "UPDATE `afv_points_batch` SET `frozen` = `frozen` - ? WHERE `id` = ?"
			if _, err := s.ExecCtx(ctx, upd, take, b.Id); err != nil {
				return nil, err
			}
			out.consumed = append(out.consumed, lineDelta{
				BatchID: b.Id, Amount: -take,
				BeforeRemaining: b.Remaining, AfterRemaining: b.Remaining,
				BeforeFrozen: b.Frozen, AfterFrozen: b.Frozen - take,
			})
			b.Frozen -= take
			frozen -= take
			leftC -= take
		}
		if leftB > 0 && frozen > 0 {
			give := min64(frozen, leftB)
			const upd = "UPDATE `afv_points_batch` SET `frozen` = `frozen` - ?, `remaining` = `remaining` + ? WHERE `id` = ?"
			if _, err := s.ExecCtx(ctx, upd, give, give, b.Id); err != nil {
				return nil, err
			}
			out.returned = append(out.returned, lineDelta{
				BatchID: b.Id, Amount: give,
				BeforeRemaining: b.Remaining, AfterRemaining: b.Remaining + give,
				BeforeFrozen: b.Frozen, AfterFrozen: b.Frozen - give,
			})
			leftB -= give
			touchedBack = true
		}
		// 退回过的批次置 ACTIVE（含 EXPIRED）；点数用完了的置 EXHAUSTED
		var err error
		if touchedBack {
			_, err = s.ExecCtx(ctx, "UPDATE `afv_points_batch` SET `status` = 'ACTIVE' WHERE `id` = ?", b.Id)
		} else {
			_, err = s.ExecCtx(ctx, "UPDATE `afv_points_batch` SET `status` = 'EXHAUSTED' "+
				"WHERE `id` = ? AND `remaining` = 0 AND `frozen` = 0 AND `status` = 'ACTIVE'", b.Id)
		}
		if err != nil {
			return nil, err
		}
	}
	if leftC > 0 || leftB > 0 {
		return nil, fmt.Errorf("billing: 用户 %d 冻结额不足以解冻（还差确认 %d / 退回 %d）", res.UserId, leftC, leftB)
	}
	return out, nil
}

// reservationRow 预留单。
type reservationRow struct {
	Id           int64          `db:"id"`
	No           string         `db:"reservation_no"`
	UserId       int64          `db:"user_id"`
	PricePoints  int64          `db:"price_points"`
	BizId        string         `db:"biz_id"`
	Status       string         `db:"status"`
	FeatureCode  string         `db:"feature_code"`
	PriceSnapsht sql.NullString `db:"price_snapshot"`
	IdemKey      string         `db:"idempotency_key"`
}

const resCols = "`id`,`reservation_no`,`user_id`,`price_points`,`biz_id`,`status`,`feature_code`,`price_snapshot`,`idempotency_key`"

func lockReservation(ctx context.Context, s sqlx.Session, no string) (*reservationRow, error) {
	var r reservationRow
	const q = "SELECT " + resCols + " FROM `afv_billing_reservation` WHERE `reservation_no` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE"
	switch err := s.QueryRowCtx(ctx, &r, q, no); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, fmt.Errorf("%w: 找不到 %s", ErrGone, no)
	case err != nil:
		return nil, err
	}
	return &r, nil
}

// loadHeld 按 biz_id 捞回还在 HELD 的单（轮询器结算时用）。
func loadHeld(ctx context.Context, conn sqlx.SqlConn, bizID string) (*Reservation, error) {
	var r reservationRow
	const q = "SELECT " + resCols + " FROM `afv_billing_reservation` " +
		"WHERE `biz_type` = ? AND `biz_id` = ? AND `status` = ? AND `deleted` = 0 LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &r, q, bizTypeLocalLife, bizID, statusHeld); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, nil // 没有在途的单，调用方跳过结算
	case err != nil:
		return nil, err
	}
	return toReservation(&r), nil
}

func loadHeldOrAny(ctx context.Context, conn sqlx.SqlConn, bizID string) (*Reservation, error) {
	var r reservationRow
	const q = "SELECT " + resCols + " FROM `afv_billing_reservation` " +
		"WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 ORDER BY `id` LIMIT 1"
	if err := conn.QueryRowCtx(ctx, &r, q, bizTypeLocalLife, bizID); err != nil {
		return nil, err
	}
	return toReservation(&r), nil
}

// toReservation 把库里的单还原成 *Reservation，顺带把快照里的结算方式取回来
// （部分成功要靠它判断能不能按实际结算）。
func toReservation(r *reservationRow) *Reservation {
	out := &Reservation{No: r.No, PricePoints: r.PricePoints}
	if r.PriceSnapsht.Valid {
		var snap struct {
			Settlement     string `json:"settlement"`
			Quantity       int    `json:"quantity"`
			EntryCode      string `json:"entryCode"`
			ModelGroup     string `json:"modelGroup"`
			VariantCode    string `json:"variantCode"`
			CarrierModelID int64  `json:"carrierModelId"`
		}
		if err := jsonUnmarshal(r.PriceSnapsht.String, &snap); err == nil {
			out.Quote = &Quote{Settlement: snap.Settlement, Quantity: snap.Quantity, EntryCode: snap.EntryCode,
				ModelGroup: snap.ModelGroup, VariantCode: snap.VariantCode, CarrierModelID: snap.CarrierModelID}
			out.variantCode, out.carrierModelID = snap.VariantCode, snap.CarrierModelID
		}
	}
	return out
}

// ---- 账本写入 ----

type logEntry struct {
	UserID                    int64
	ChangeType                string
	Amount                    int64
	BalanceAfter, FrozenAfter int64
	BizType, BizID            string
	IdempotencyKey            string
	Description               string
	// Operator 操作人。照 Java：冻结记 USER/{userId}，确认与退回记 SYSTEM/null。
	OperatorType string
	OperatorID   sql.NullString
}

func writeLog(ctx context.Context, s sqlx.Session, e logEntry) (int64, error) {
	op := e.OperatorType
	if op == "" {
		op = "SYSTEM"
	}
	const q = "INSERT INTO `afv_points_log` " +
		"(`ledger_no`,`user_id`,`change_type`,`amount`,`balance_after`,`frozen_after`," +
		"`biz_type`,`biz_id`,`idempotency_key`,`operator_type`,`operator_id`,`description`) " +
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"
	r, err := s.ExecCtx(ctx, q, newNo("PL"), e.UserID, e.ChangeType, e.Amount,
		e.BalanceAfter, e.FrozenAfter, e.BizType, e.BizID, e.IdempotencyKey, op, e.OperatorID, e.Description)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// lineDelta 一个批次上的变动，写进 afv_points_log_line。
type lineDelta struct {
	BatchID                         int64
	Amount                          int64
	BeforeRemaining, AfterRemaining int64
	BeforeFrozen, AfterFrozen       int64
}

func writeLogLines(ctx context.Context, s sqlx.Session, ledgerID, userID int64, lines []lineDelta) error {
	const q = "INSERT INTO `afv_points_log_line` " +
		"(`ledger_id`,`batch_id`,`user_id`,`amount`,`before_remaining`,`after_remaining`,`before_frozen`,`after_frozen`) " +
		"VALUES (?,?,?,?,?,?,?,?)"
	for _, l := range lines {
		if _, err := s.ExecCtx(ctx, q, ledgerID, l.BatchID, userID, l.Amount,
			l.BeforeRemaining, l.AfterRemaining, l.BeforeFrozen, l.AfterFrozen); err != nil {
			return err
		}
	}
	return nil
}

func writeResLines(ctx context.Context, s sqlx.Session, no string, q *Quote) error {
	const ins = "INSERT INTO `afv_billing_reservation_line` " +
		"(`reservation_no`,`line_no`,`component_code`,`step_code`,`mode`,`reserved_points`,`status`) " +
		"VALUES (?,?,?,?,?,?,?)"
	n := 0
	for _, l := range q.Lines {
		if l.CostOnly {
			continue
		}
		n++
		if _, err := s.ExecCtx(ctx, ins, no, n, l.ComponentCode, l.StepCode, l.Mode, l.Points, statusHeld); err != nil {
			return err
		}
	}
	return nil
}

func nullInt(i int64) sql.NullInt64 { return sql.NullInt64{Int64: i, Valid: i != 0} }

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
