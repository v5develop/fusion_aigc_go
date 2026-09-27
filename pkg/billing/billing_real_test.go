package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 连真实的 fusion_aigc 跑一遍完整账务。默认跳过：
//
//	BILLING_REAL=1 BILLING_DSN='root:123456@tcp(127.0.0.1:3306)/fusion_aigc?parseTime=true' \
//	  go test ./pkg/billing -run Real -v
//
// 测试自己造配置（offer/variant/component/pricing）和账户，跑完全部删掉。
// 用户 ID 一律 999 开头，不会碰到真实用户。

const (
	testUser      = 999000001
	testEntryAll  = "__billingTestAll"     // ALL_OR_NOTHING，按秒
	testEntryPart = "__billingTestPartial" // PARTIAL，按张
	testOfferAll  = "__billing_test_all"
	testOfferPart = "__billing_test_part"
	testModelCode = "__billing_test_model"
)

func openDB(t *testing.T) sqlx.SqlConn {
	t.Helper()
	dsn := os.Getenv("BILLING_DSN")
	if os.Getenv("BILLING_REAL") != "1" || dsn == "" {
		t.Skip("设置 BILLING_REAL=1 与 BILLING_DSN 后才会连真实的库")
	}
	return sqlx.NewMysql(dsn)
}

// setup 造配置和账户。返回清理函数。
func setup(t *testing.T, conn sqlx.SqlConn, balance int64) func() {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecCtx(ctx, q, args...); err != nil {
			t.Fatalf("setup %s: %v", q, err)
		}
	}
	cleanup(conn) // 上次没跑完留下的先清掉

	// 模型 + 单价：按秒 10 点、按张 3 点
	exec("INSERT INTO afv_ai_model (code, name, model_type, status, deleted) VALUES (?,?,?,1,0)",
		testModelCode, "计费测试模型", 4)
	var modelID int64
	if err := conn.QueryRowCtx(ctx, &modelID, "SELECT id FROM afv_ai_model WHERE code = ? AND deleted = 0", testModelCode); err != nil {
		t.Fatalf("取测试模型: %v", err)
	}
	exec("INSERT INTO afv_model_pricing (model_id, action_type, price_points, price_unit, status, deleted) VALUES (?,?,?,?,1,0)",
		modelID, "VIDEO_GEN", 10, "PER_SECOND")
	exec("INSERT INTO afv_model_pricing (model_id, action_type, price_points, price_unit, status, deleted) VALUES (?,?,?,?,1,0)",
		modelID, "IMAGE_GEN", 3, "PER_IMAGE")

	// 两个 offer：一个 ALL_OR_NOTHING 按秒，一个 PARTIAL 按张
	exec("INSERT INTO afv_billing_offer (offer_code, name, entry_code, feature_code, status, deleted) VALUES (?,?,?,?,'ENABLED',0)",
		testOfferAll, "计费测试-整单", testEntryAll, "ecom_video_gen")
	exec("INSERT INTO afv_billing_offer (offer_code, name, entry_code, feature_code, status, deleted) VALUES (?,?,?,?,'ENABLED',0)",
		testOfferPart, "计费测试-部分", testEntryPart, "ecom_image_gen")
	exec("INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, default_model_id, settlement_type, status, deleted) VALUES (?,?,?,?,?,'ENABLED',0)",
		testOfferAll+"_v", testOfferAll, "整单档", modelID, "ALL_OR_NOTHING")
	exec("INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, default_model_id, settlement_type, status, deleted) VALUES (?,?,?,?,?,'ENABLED',0)",
		testOfferPart+"_v", testOfferPart, "部分档", modelID, "PARTIAL")
	exec("INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted) VALUES (?,?,?,?,85,?,?,?,0,1.0,0,0)",
		testOfferAll+"_c", testOfferAll+"_v", "video_gen", "DURATION", "BILLED", "VIDEO_GEN", modelID)
	exec("INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted) VALUES (?,?,?,?,85,?,?,?,0,1.0,0,0)",
		testOfferPart+"_c", testOfferPart+"_v", "image_gen", "OUTPUT", "BILLED", "IMAGE_GEN", modelID)

	// 账户 + 两个批次（一个先过期，验证 FIFO 顺序）
	exec("INSERT INTO afv_points_account (user_id, balance, frozen_balance, total_received, version, deleted) VALUES (?,?,0,?,0,0)",
		testUser, balance, balance)
	var accID int64
	if err := conn.QueryRowCtx(ctx, &accID, "SELECT id FROM afv_points_account WHERE user_id = ?", testUser); err != nil {
		t.Fatalf("取测试账户: %v", err)
	}
	half := balance / 2
	exec("INSERT INTO afv_points_batch (account_id, user_id, amount, remaining, frozen, source, expire_time, status, deleted) VALUES (?,?,?,?,0,'ADMIN_GIFT',?, 'ACTIVE',0)",
		accID, testUser, half, half, time.Now().Add(24*time.Hour))
	exec("INSERT INTO afv_points_batch (account_id, user_id, amount, remaining, frozen, source, expire_time, status, deleted) VALUES (?,?,?,?,0,'ADMIN_GIFT',NULL,'ACTIVE',0)",
		accID, testUser, balance-half, balance-half)

	return func() { cleanup(conn) }
}

func cleanup(conn sqlx.SqlConn) {
	ctx := context.Background()
	for _, q := range []string{
		"DELETE l FROM afv_points_log_line l JOIN afv_points_log g ON g.id = l.ledger_id WHERE g.user_id = " + fmt.Sprint(testUser),
		"DELETE FROM afv_points_log WHERE user_id = " + fmt.Sprint(testUser),
		"DELETE rl FROM afv_billing_reservation_line rl JOIN afv_billing_reservation r ON r.reservation_no = rl.reservation_no WHERE r.user_id = " + fmt.Sprint(testUser),
		"DELETE FROM afv_billing_reservation WHERE user_id = " + fmt.Sprint(testUser),
		"DELETE FROM afv_points_batch WHERE user_id = " + fmt.Sprint(testUser),
		"DELETE FROM afv_points_account WHERE user_id = " + fmt.Sprint(testUser),
		"DELETE FROM afv_billing_charge_component WHERE component_code LIKE '__billing_test%'",
		"DELETE FROM afv_billing_offer_variant WHERE offer_code LIKE '__billing_test%'",
		"DELETE FROM afv_billing_offer WHERE offer_code LIKE '__billing_test%'",
		"DELETE p FROM afv_model_pricing p JOIN afv_ai_model m ON m.id = p.model_id WHERE m.code = '" + testModelCode + "'",
		"DELETE FROM afv_ai_model WHERE code = '" + testModelCode + "'",
	} {
		_, _ = conn.ExecCtx(ctx, q)
	}
}

// acct 读账户余额与冻结。
func acct(t *testing.T, conn sqlx.SqlConn) (balance, frozen int64) {
	t.Helper()
	var a Account
	if err := conn.QueryRowCtx(context.Background(), &a,
		"SELECT "+accountCols+" FROM afv_points_account WHERE user_id = ?", testUser); err != nil {
		t.Fatalf("读账户: %v", err)
	}
	return a.Balance, a.FrozenBalance
}

// batchSum 批次的 remaining / frozen 汇总，用来验证批次和账户一致。
func batchSum(t *testing.T, conn sqlx.SqlConn) (remaining, frozen int64) {
	t.Helper()
	var r struct {
		Remaining int64 `db:"remaining"`
		Frozen    int64 `db:"frozen"`
	}
	if err := conn.QueryRowCtx(context.Background(), &r,
		"SELECT COALESCE(SUM(remaining),0) remaining, COALESCE(SUM(frozen),0) frozen FROM afv_points_batch WHERE user_id = ?", testUser); err != nil {
		t.Fatalf("读批次: %v", err)
	}
	return r.Remaining, r.Frozen
}

func want(t *testing.T, name string, got, expect int64) {
	t.Helper()
	if got != expect {
		t.Fatalf("%s: got %d, want %d", name, got, expect)
	}
}

// checkConsistent 账户和批次必须始终一致——这是整套东西的底线。
func checkConsistent(t *testing.T, conn sqlx.SqlConn) {
	t.Helper()
	b, f := acct(t, conn)
	br, bf := batchSum(t, conn)
	want(t, "账户余额 vs 批次 remaining", br, b)
	want(t, "账户冻结 vs 批次 frozen", bf, f)
}

func TestRealFreezeConfirm(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	// 15 秒 × 10 点 = 150
	c := Charge{UserID: testUser, RequestID: "req-confirm-1", EntryCode: testEntryAll, Kind: KindVideo, Seconds: 15}
	q, err := s.Quote(ctx, c)
	if err != nil {
		t.Fatalf("算价: %v", err)
	}
	want(t, "报价", q.PricePoints, 150)
	if q.PriceUnit != "PER_SECOND" {
		t.Fatalf("计价单位: %s", q.PriceUnit)
	}

	r, err := s.Freeze(ctx, c)
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	want(t, "冻结点数", r.PricePoints, 150)
	b, f := acct(t, conn)
	want(t, "冻结后余额", b, 850)
	want(t, "冻结后冻结额", f, 150)
	checkConsistent(t, conn)

	// FIFO：先过期的批次（500）先被扣
	var firstFrozen int64
	if err := conn.QueryRowCtx(ctx, &firstFrozen,
		"SELECT frozen FROM afv_points_batch WHERE user_id = ? AND expire_time IS NOT NULL", testUser); err != nil {
		t.Fatalf("读首批: %v", err)
	}
	want(t, "先过期的批次先扣", firstFrozen, 150)

	// 幂等：同一个 RequestID 再冻一次，不重复扣
	r2, err := s.Freeze(ctx, c)
	if err != nil {
		t.Fatalf("重复冻结: %v", err)
	}
	want(t, "幂等返回同一单", int64(btoi(r2.No == r.No)), 1)
	b, f = acct(t, conn)
	want(t, "重复冻结后余额不变", b, 850)
	want(t, "重复冻结后冻结额不变", f, 150)

	// 确认：frozen 清零，balance 不动
	if err := s.Confirm(ctx, r, 0); err != nil {
		t.Fatalf("确认: %v", err)
	}
	b, f = acct(t, conn)
	want(t, "确认后余额（不再变）", b, 850)
	want(t, "确认后冻结额", f, 0)
	checkConsistent(t, conn)

	// 重复确认要幂等
	if err := s.Confirm(ctx, r, 0); err != nil {
		t.Fatalf("重复确认: %v", err)
	}

	// 账本：FREEZE -150 / CONSUME -150，且 CONSUME 的 balance_after 和 FREEZE 相同
	var logs []struct {
		ChangeType   string `db:"change_type"`
		Amount       int64  `db:"amount"`
		BalanceAfter int64  `db:"balance_after"`
		FrozenAfter  int64  `db:"frozen_after"`
	}
	if err := conn.QueryRowsCtx(ctx, &logs,
		"SELECT change_type, amount, balance_after, frozen_after FROM afv_points_log WHERE user_id = ? ORDER BY id", testUser); err != nil {
		t.Fatalf("读账本: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("账本应有 2 行，实际 %d", len(logs))
	}
	if logs[0].ChangeType != changeFreeze || logs[0].Amount != -150 || logs[0].FrozenAfter != 150 {
		t.Fatalf("FREEZE 行不对: %+v", logs[0])
	}
	if logs[1].ChangeType != changeConsume || logs[1].Amount != -150 || logs[1].FrozenAfter != 0 {
		t.Fatalf("CONSUME 行不对: %+v", logs[1])
	}
	want(t, "CONSUME 的 balance_after 与 FREEZE 一致（钱在冻结时已出余额）",
		logs[1].BalanceAfter, logs[0].BalanceAfter)
}

func TestRealRefund(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-refund-1",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 30})
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	want(t, "30 秒 = 300 点", r.PricePoints, 300)

	if err := s.Refund(ctx, r, "生成失败"); err != nil {
		t.Fatalf("退回: %v", err)
	}
	b, f := acct(t, conn)
	want(t, "退回后余额复原", b, 1000)
	want(t, "退回后冻结额", f, 0)
	checkConsistent(t, conn)

	if err := s.Refund(ctx, r, "重复退"); err != nil {
		t.Fatalf("重复退回应幂等: %v", err)
	}
	b, _ = acct(t, conn)
	want(t, "重复退回余额不变", b, 1000)
}

func TestRealPartialConfirm(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	// 4 张 × 3 点 = 12，实际只成功 3 张 → 扣 9 退 3
	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-partial-1",
		EntryCode: testEntryPart, Count: 4})
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	want(t, "4 张 = 12 点", r.PricePoints, 12)

	if err := s.Confirm(ctx, r, 3); err != nil {
		t.Fatalf("部分确认: %v", err)
	}
	b, f := acct(t, conn)
	want(t, "部分成功后余额（退回 3）", b, 991)
	want(t, "部分成功后冻结额", f, 0)
	checkConsistent(t, conn)

	// 配平：FREEZE = CONSUME + REFUND
	var sum struct {
		Freeze  int64 `db:"freeze_sum"`
		Consume int64 `db:"consume_sum"`
		Refund  int64 `db:"refund_sum"`
	}
	if err := conn.QueryRowCtx(ctx, &sum,
		"SELECT COALESCE(-SUM(IF(change_type='FREEZE',amount,0)),0) freeze_sum,"+
			" COALESCE(-SUM(IF(change_type='CONSUME',amount,0)),0) consume_sum,"+
			" COALESCE(SUM(IF(change_type='REFUND',amount,0)),0) refund_sum"+
			" FROM afv_points_log WHERE user_id = ?", testUser); err != nil {
		t.Fatalf("配平查询: %v", err)
	}
	want(t, "配平 FREEZE = CONSUME + REFUND", sum.Freeze, sum.Consume+sum.Refund)
	want(t, "实际消耗", sum.Consume, 9)
	want(t, "退回", sum.Refund, 3)
}

// ALL_OR_NOTHING 的档位即使只成功一部分也全额扣——这是 Java 的配置语义，不能自己放宽。
func TestRealAllOrNothingIgnoresPartial(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-aon-1",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 10})
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	if err := s.Confirm(ctx, r, 5); err != nil { // 传实际 5 秒，但档位不允许部分结算
		t.Fatalf("确认: %v", err)
	}
	b, _ := acct(t, conn)
	want(t, "ALL_OR_NOTHING 全额扣", b, 900)
}

func TestRealNotEnough(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 50)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	_, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-poor-1",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 15}) // 要 150，只有 50
	if !errors.Is(err, ErrNotEnough) {
		t.Fatalf("应该是余额不足，实际 %v", err)
	}
	var ne *NotEnough
	if !errors.As(err, &ne) {
		t.Fatalf("应该能取出差额")
	}
	want(t, "还差", ne.Missing(), 100)

	b, f := acct(t, conn)
	want(t, "失败后余额不变", b, 50)
	want(t, "失败后无冻结", f, 0)

	// 事务要整笔回滚：预留单也不能留下
	var n int64
	if err := conn.QueryRowCtx(ctx, &n,
		"SELECT COUNT(*) FROM afv_billing_reservation WHERE user_id = ?", testUser); err != nil {
		t.Fatalf("查预留单: %v", err)
	}
	want(t, "余额不足不留预留单", n, 0)
}

// 回收器只收自己的单，不碰 Java 写的。
func TestRealReclaimOnlyOurs(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-expire-1",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 10})
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	// 把它改成已过期
	if _, err := conn.ExecCtx(ctx,
		"UPDATE afv_billing_reservation SET expire_time = NOW() - INTERVAL 1 MINUTE WHERE reservation_no = ?", r.No); err != nil {
		t.Fatalf("改过期时间: %v", err)
	}
	// 再造一条"Java 写的"过期单，回收器不该碰
	if _, err := conn.ExecCtx(ctx,
		"UPDATE afv_billing_reservation SET idempotency_key = ? WHERE reservation_no = ?",
		"doubao:not-ours", r.No+"-none"); err != nil {
		t.Fatalf("准备对照单: %v", err)
	}
	other, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-expire-2",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 10})
	if err != nil {
		t.Fatalf("冻结对照单: %v", err)
	}
	if _, err := conn.ExecCtx(ctx,
		"UPDATE afv_billing_reservation SET expire_time = NOW() - INTERVAL 1 MINUTE, idempotency_key = ? WHERE reservation_no = ?",
		"doubao:"+other.No, other.No); err != nil {
		t.Fatalf("改对照单: %v", err)
	}

	n := NewReclaimer(conn, time.Minute).Tick(ctx)
	want(t, "只回收自己的一条", int64(n), 1)

	var st string
	if err := conn.QueryRowCtx(ctx, &st,
		"SELECT status FROM afv_billing_reservation WHERE reservation_no = ?", r.No); err != nil {
		t.Fatalf("查状态: %v", err)
	}
	if st != statusRefunded {
		t.Fatalf("我们的单应该被退回，实际 %s", st)
	}
	if err := conn.QueryRowCtx(ctx, &st,
		"SELECT status FROM afv_billing_reservation WHERE reservation_no = ?", other.No); err != nil {
		t.Fatalf("查对照单: %v", err)
	}
	if st != statusHeld {
		t.Fatalf("Java 的单不该被我们动，实际 %s", st)
	}
	checkConsistent(t, conn)
}

// Load：冻结在提交接口、确认在轮询器，中间隔着进程重启，只能靠库把单子捞回来。
func TestRealLoadForSettle(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	if _, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-load-1",
		EntryCode: testEntryPart, Count: 4}); err != nil {
		t.Fatalf("冻结: %v", err)
	}
	// 模拟重启：只拿 userID + requestID
	got, err := s.Load(ctx, testUser, "req-load-1")
	if err != nil || got == nil {
		t.Fatalf("捞回预留单: %v %v", got, err)
	}
	want(t, "捞回的点数", got.PricePoints, 12)
	if got.Quote == nil || got.Quote.Settlement != "PARTIAL" {
		t.Fatalf("结算方式要能从快照里还原，实际 %+v", got.Quote)
	}
	// 捞回来的单能按实际结算
	if err := s.Confirm(ctx, got, 2); err != nil {
		t.Fatalf("确认: %v", err)
	}
	b, _ := acct(t, conn)
	want(t, "按 2 张结算", b, 994)
	checkConsistent(t, conn)
}

func TestRealModeOff(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOff}, conn)

	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-off-1",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 15})
	if err != nil || r != nil {
		t.Fatalf("off 模式应该什么都不做: %v %v", r, err)
	}
	if err := s.Confirm(ctx, r, 0); err != nil {
		t.Fatalf("off 模式确认: %v", err)
	}
	if err := s.Refund(ctx, r, ""); err != nil {
		t.Fatalf("off 模式退回: %v", err)
	}
	b, f := acct(t, conn)
	want(t, "off 模式余额不变", b, 1000)
	want(t, "off 模式无冻结", f, 0)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
