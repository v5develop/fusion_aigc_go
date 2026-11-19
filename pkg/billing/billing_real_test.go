package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
	testRuleCode  = "__billing_test_rule"
	testEntryText = "__billingTestText" // 按次
	testOfferText = "__billing_test_text"
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
	// 按次（AI文案的口径）：1 积分/次
	exec("INSERT INTO afv_model_pricing (model_id, action_type, price_points, price_unit, status, deleted) VALUES (?,?,?,?,1,0)",
		modelID, "TEXT_GEN", 1, "PER_REQUEST")

	// 两个 offer：一个 ALL_OR_NOTHING 按秒，一个 PARTIAL 按张
	exec("INSERT INTO afv_billing_offer (offer_code, name, entry_code, feature_code, status, deleted) VALUES (?,?,?,?,'ENABLED',0)",
		testOfferAll, "计费测试-整单", testEntryAll, "ecom_video_gen")
	exec("INSERT INTO afv_billing_offer (offer_code, name, entry_code, feature_code, status, deleted) VALUES (?,?,?,?,'ENABLED',0)",
		testOfferPart, "计费测试-部分", testEntryPart, "ecom_image_gen")
	exec("INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, default_model_id, settlement_type, status, deleted) VALUES (?,?,?,?,?,'ENABLED',0)",
		testOfferAll+"_cloud", testOfferAll, "云端档", modelID, "ALL_OR_NOTHING")
	exec("INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, default_model_id, settlement_type, status, deleted) VALUES (?,?,?,?,?,'ENABLED',0)",
		testOfferPart+"_v", testOfferPart, "部分档", modelID, "PARTIAL")
	// 按次的 offer：AI文案就是这个形状
	exec("INSERT INTO afv_billing_offer (offer_code, name, entry_code, feature_code, status, deleted) VALUES (?,?,?,?,'ENABLED',0)",
		testOfferText, "计费测试-按次", testEntryText, "ai_chat")
	exec("INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, default_model_id, settlement_type, status, deleted) VALUES (?,?,?,?,?,'ENABLED',0)",
		testOfferText+"_v", testOfferText, "按次档", modelID, "ALL_OR_NOTHING")
	exec("INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted) VALUES (?,?,?,?,77,?,?,?,0,1.0,0,0)",
		testOfferText+"_c", testOfferText+"_v", "text_gen", "BASE", "BILLED", "TEXT_GEN", modelID)

	// 再给"整单"那个 offer 建一个 lite 档：档位码是 <offer_code>_lite，验证按档次选
	exec("INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, default_model_id, quality_tier, settlement_type, status, deleted) VALUES (?,?,?,?,?,?,'ENABLED',0)",
		testOfferAll+"_lite", testOfferAll, "lite档", modelID, "lite", "ALL_OR_NOTHING")
	exec("INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted) VALUES (?,?,?,?,85,?,?,?,0,1.0,0,0)",
		testOfferAll+"_c", testOfferAll+"_cloud", "video_gen", "DURATION", "BILLED", "VIDEO_GEN", modelID)
	exec("INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted) VALUES (?,?,?,?,85,?,?,?,0,1.0,0,0)",
		testOfferPart+"_c", testOfferPart+"_v", "image_gen", "OUTPUT", "BILLED", "IMAGE_GEN", modelID)
	// lite 档的步骤：固定 4 点/秒，和云端的 10 点/秒区分开
	// 这个步骤故意不给 action_type：照 Java 的语义，查不到单价行才退回 base_points
	exec("INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted) VALUES (?,?,?,?,45,?,NULL,NULL,4,1.0,0,0)",
		testOfferAll+"_lite_c", testOfferAll+"_lite", "video_gen", "DURATION", "BILLED")

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
		"DELETE FROM afv_billing_charge_rule WHERE rule_code = '" + testRuleCode + "'",
		"DELETE FROM afv_app_user_feature WHERE user_id = " + fmt.Sprint(testUser),
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
	want(t, "新冻结不是命中旧单", int64(btoi(r.Replayed())), 0)
	want(t, "幂等命中旧单要标出来", int64(btoi(r2.Replayed())), 1)
	if r2.Quote == nil || r2.Quote.EntryCode != testEntryAll {
		t.Fatalf("命中旧单应带回价格快照的入口: %+v", r2.Quote)
	}
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

	// 终态与幂等键都要和 Java 一致
	var st string
	if err := conn.QueryRowCtx(ctx, &st,
		"SELECT status FROM afv_billing_reservation WHERE reservation_no = ?", r.No); err != nil {
		t.Fatalf("查状态: %v", err)
	}
	if st != statusPartiallyRefunded {
		t.Fatalf("部分结算终态应为 PARTIALLY_REFUNDED，实际 %s", st)
	}
	var keys []string
	if err := conn.QueryRowsCtx(ctx, &keys,
		"SELECT idempotency_key FROM afv_points_log WHERE user_id = ? AND change_type IN (?,?) ORDER BY id",
		testUser, changeConsume, changeRefund); err != nil {
		t.Fatalf("查幂等键: %v", err)
	}
	for i, wantKey := range []string{"partial-consume:" + r.No, "partial-refund:" + r.No} {
		if i >= len(keys) || keys[i] != wantKey {
			t.Fatalf("幂等键第 %d 个应为 %s，实际 %v", i+1, wantKey, keys)
		}
	}
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

// 计量要看 price_unit，不看 component_type。
//
// 现有配置里真的有打架的：action_transfer 的步骤写的是 OUTPUT（按产出数量），
// 单价却是 PER_SECOND（按秒）。按 component_type 算会把 15 秒算成 1 次，差 15 倍。
func TestRealUnitBeatsComponentType(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	// 把按秒那档的步骤类型改成 OUTPUT，制造和 action_transfer 一样的矛盾配置
	if _, err := conn.ExecCtx(ctx,
		"UPDATE afv_billing_charge_component SET component_type = 'OUTPUT' WHERE component_code = ?",
		testOfferAll+"_c"); err != nil {
		t.Fatalf("改步骤类型: %v", err)
	}
	q, err := s.Quote(ctx, Charge{UserID: testUser, RequestID: "req-unit-1",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 15})
	if err != nil {
		t.Fatalf("算价: %v", err)
	}
	want(t, "仍按秒算 15×10", q.PricePoints, 150)
	if q.PriceUnit != "PER_SECOND" {
		t.Fatalf("计价单位应取单价表的，实际 %s", q.PriceUnit)
	}
}

// 按档次选档位。模型码两边对不上（我们是 seedance2.5/lite，计费配置里是
// doubao-seedance-2-5/xiaoqi-video-1.0-lite-b6），所以对接键用 Tier。
func TestRealPickByTier(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	cloud, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryAll,
		Tier: TierCloud, Kind: KindVideo, Seconds: 15})
	if err != nil {
		t.Fatalf("云端档算价: %v", err)
	}
	want(t, "云端 15×10", cloud.PricePoints, 150)

	lite, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryAll,
		Tier: TierLite, Kind: KindVideo, Seconds: 15})
	if err != nil {
		t.Fatalf("lite 档算价: %v", err)
	}
	want(t, "lite 走 base_points 15×4", lite.PricePoints, 60)
	if lite.VariantCode == cloud.VariantCode {
		t.Fatalf("两档选到了同一个档位: %s", lite.VariantCode)
	}

	// 没有的档次要明确报错，不能退回第一个档位按云端价扣
	if _, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryAll,
		Tier: "ultra", Kind: KindVideo, Seconds: 15}); !errors.Is(err, ErrNoOffer) {
		t.Fatalf("不存在的档次应该报 ErrNoOffer，实际 %v", err)
	}
}

// 有已发布的计价规则命中我们时必须拒绝算价，不能算出一个和 Java 不一样的价。
// Java 的规则引擎有 6 个因子（含要读充值记录打分的 userLevel），我们没复刻。
func TestRealRefusesWhenPricingRuleExists(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	// 先确认没有规则时算得出来
	if _, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryAll,
		Tier: TierCloud, Kind: KindVideo, Seconds: 15}); err != nil {
		t.Fatalf("没有规则时应该算得出来: %v", err)
	}

	// 发布一条通配规则（库里那两条现成的就是 scope_code='*'，只是停在 DRAFT）
	if _, err := conn.ExecCtx(ctx,
		"INSERT INTO afv_billing_charge_rule (rule_code, scope_type, scope_code, factor_key, operator,"+
			" match_value, effect_type, effect_value, priority, status, version, deleted)"+
			" VALUES (?,'OFFER','*','userLevel','EQ','\"LV3\"','DISCOUNT',90.0,10,'PUBLISHED',1,0)",
		testRuleCode); err != nil {
		t.Fatalf("造规则: %v", err)
	}
	defer func() {
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_billing_charge_rule WHERE rule_code = ?", testRuleCode)
	}()

	_, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryAll,
		Tier: TierCloud, Kind: KindVideo, Seconds: 15})
	if !errors.Is(err, ErrPricingRule) {
		t.Fatalf("有已发布规则时应该拒绝算价，实际 %v", err)
	}
	// 冻结也要一起挡住，不能只拦报价
	if _, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-rule-1",
		EntryCode: testEntryAll, Tier: TierCloud, Kind: KindVideo, Seconds: 15}); !errors.Is(err, ErrPricingRule) {
		t.Fatalf("冻结也应该被挡住，实际 %v", err)
	}
	b, f := acct(t, conn)
	want(t, "被挡住后余额不变", b, 1000)
	want(t, "被挡住后无冻结", f, 0)
}

// 权益闸门：Java 的这道闸在报价那步，我们直接写表绕过了，所以做成可开关的。
func TestRealEntitlementGate(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	c := Charge{UserID: testUser, RequestID: "req-feat-1", EntryCode: testEntryAll,
		Tier: TierCloud, Kind: KindVideo, Seconds: 15}

	// 关掉时：没有权益也能冻结
	if _, err := New(Conf{Mode: ModeOn}, conn).Freeze(ctx, c); err != nil {
		t.Fatalf("关掉时不查权益: %v", err)
	}

	// 打开后：没有权益要被拒
	c.RequestID = "req-feat-2"
	strict := New(Conf{Mode: ModeOn, CheckEntitlement: true}, conn)
	if _, err := strict.Freeze(ctx, c); !errors.Is(err, ErrNoFeature) {
		t.Fatalf("打开后没有权益应该被拒，实际 %v", err)
	}

	// 发一个权益（测试 offer 的 feature_code 是 ecom_video_gen），再试
	if _, err := conn.ExecCtx(ctx,
		"INSERT INTO afv_app_user_feature (user_id, feature_code, status, source, deleted)"+
			" VALUES (?,?,'ACTIVE','MANUAL',0)", testUser, "ecom_video_gen"); err != nil {
		t.Fatalf("发权益: %v", err)
	}
	defer func() {
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_app_user_feature WHERE user_id = ?", testUser)
	}()
	if _, err := strict.Freeze(ctx, c); err != nil {
		t.Fatalf("有权益后应该能冻结: %v", err)
	}
}

// 权益是准入闸，不是钱：Mode=off（不扣点）时也要照样拦。
func TestRealAccessGateIndependentOfBilling(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	// 不扣点，但开权益
	s := New(Conf{Mode: ModeOff, CheckEntitlement: true}, conn)

	if err := s.CheckAccess(ctx, testUser, testEntryAll); !errors.Is(err, ErrNoFeature) {
		t.Fatalf("没有权益应该被拦，实际 %v", err)
	}
	if _, err := conn.ExecCtx(ctx,
		"INSERT INTO afv_app_user_feature (user_id, feature_code, status, source, deleted)"+
			" VALUES (?,?,'ACTIVE','UNION',0)", testUser, "ecom_video_gen"); err != nil {
		t.Fatalf("发权益: %v", err)
	}
	defer func() {
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_app_user_feature WHERE user_id = ?", testUser)
	}()
	if err := s.CheckAccess(ctx, testUser, testEntryAll); err != nil {
		t.Fatalf("有权益后应该放行: %v", err)
	}

	// 没配计费方案的入口码：放行，不能因为计费没配好就把功能拦死
	if err := s.CheckAccess(ctx, testUser, "__nonexistent_entry"); err != nil {
		t.Fatalf("没配方案的入口码应该放行: %v", err)
	}
	// 关掉开关就一律放行
	if err := New(Conf{Mode: ModeOff}, conn).CheckAccess(ctx, testUser, testEntryPart); err != nil {
		t.Fatalf("关掉开关应该放行: %v", err)
	}
}

// 按次计费（AI文案）：单位是 PER_REQUEST，一次就是 1 份，不看时长。
func TestRealPerRequest(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	q, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryText})
	if err != nil {
		t.Fatalf("按次算价: %v", err)
	}
	want(t, "一次 1 积分", q.PricePoints, 1)
	if q.PriceUnit != "PER_REQUEST" {
		t.Fatalf("计价单位应为 PER_REQUEST，实际 %s", q.PriceUnit)
	}
	// 传了秒数也不该影响按次的价
	q2, err := s.Quote(ctx, Charge{UserID: testUser, EntryCode: testEntryText, Seconds: 30})
	if err != nil {
		t.Fatalf("按次算价（带秒数）: %v", err)
	}
	want(t, "按次不看时长", q2.PricePoints, 1)

	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-text-1", EntryCode: testEntryText})
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	if err := s.Confirm(ctx, r, 0); err != nil {
		t.Fatalf("确认: %v", err)
	}
	b, _ := acct(t, conn)
	want(t, "扣 1 积分", b, 999)
	checkConsistent(t, conn)
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
	if st != "EXPIRED" {
		t.Fatalf("我们的单过期应该置 EXPIRED（照 Java 的 release），实际 %s", st)
	}
	var ct string
	if err := conn.QueryRowCtx(ctx, &ct,
		"SELECT change_type FROM afv_points_log WHERE biz_id = ? AND change_type = ?",
		fmt.Sprintf("%d_%s", testUser, "req-expire-1"), changeUnfreeze); err != nil {
		t.Fatalf("过期应记 UNFREEZE 流水: %v", err)
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

// 同一个用户同时有两张在途单冻在同一批次上：退其中一张，只能退回它自己冻结的那几个批次与数量，
// 不能拿别人的（Java 的单也会这样共用批次）。2026-10-10 Java 回复 2.1 / 2.2。
func TestRealRefundOnlyOwnBatches(t *testing.T) {
	conn := openDB(t)
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	// 批次 1（先过期）500、批次 2 500。A 冻 100（批次 1），B 冻 450（批次 1 的 400 + 批次 2 的 50）
	a, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "own-a", EntryCode: testEntryAll, Kind: KindVideo, Seconds: 10})
	if err != nil {
		t.Fatalf("冻结 A: %v", err)
	}
	b, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "own-b", EntryCode: testEntryAll, Kind: KindVideo, Seconds: 45})
	if err != nil {
		t.Fatalf("冻结 B: %v", err)
	}
	want(t, "A 100 点", a.PricePoints, 100)
	want(t, "B 450 点", b.PricePoints, 450)

	frozenOf := func() (b1, b2 int64) {
		var rows []struct {
			Frozen int64 `db:"frozen"`
		}
		if err := conn.QueryRowsCtx(ctx, &rows, "SELECT frozen FROM afv_points_batch WHERE user_id = ? ORDER BY (expire_time IS NULL), expire_time", testUser); err != nil || len(rows) != 2 {
			t.Fatalf("读批次: %v %d", err, len(rows))
		}
		return rows[0].Frozen, rows[1].Frozen
	}
	f1, f2 := frozenOf()
	want(t, "批次 1 冻结", f1, 500)
	want(t, "批次 2 冻结", f2, 50)

	// 批次 1 在退款前被过期任务置成 EXPIRED：退回后要回到 ACTIVE
	if _, err := conn.ExecCtx(ctx, "UPDATE afv_points_batch SET status = 'EXPIRED' WHERE user_id = ? AND expire_time IS NOT NULL", testUser); err != nil {
		t.Fatal(err)
	}
	if err := s.Refund(ctx, b, "退 B"); err != nil {
		t.Fatalf("退 B: %v", err)
	}
	f1, f2 = frozenOf()
	want(t, "退 B 后批次 1 只剩 A 的 100", f1, 100)
	want(t, "退 B 后批次 2 不再冻结", f2, 0)
	var st string
	if err := conn.QueryRowCtx(ctx, &st, "SELECT status FROM afv_points_batch WHERE user_id = ? AND expire_time IS NOT NULL", testUser); err != nil {
		t.Fatal(err)
	}
	if st != "ACTIVE" {
		t.Fatalf("退回的批次应置回 ACTIVE，实际 %s", st)
	}
	checkConsistent(t, conn)

	if err := s.Refund(ctx, a, "退 A"); err != nil {
		t.Fatalf("退 A: %v", err)
	}
	f1, f2 = frozenOf()
	want(t, "全退后批次 1 冻结", f1, 0)
	want(t, "全退后批次 2 冻结", f2, 0)
	checkConsistent(t, conn)
}

// 回收与续期并发：扫描到过期单之后、回收之前租约被续期，行锁下复核必须跳过，不能把在途任务的点数退掉。
// 只在库名含 scratch / test 时运行（会改预留单过期时间）。
func TestRealReclaimSkipsRenewedLease(t *testing.T) {
	conn := openDB(t)
	dsn := os.Getenv("BILLING_DSN")
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Skip("库名需含 scratch 或 test")
	}
	defer setup(t, conn, 1000)()
	ctx := context.Background()
	s := New(Conf{Mode: ModeOn}, conn)

	r, err := s.Freeze(ctx, Charge{UserID: testUser, RequestID: "req-renew-race",
		EntryCode: testEntryAll, Kind: KindVideo, Seconds: 10})
	if err != nil {
		t.Fatalf("冻结: %v", err)
	}
	if _, err := conn.ExecCtx(ctx,
		"UPDATE afv_billing_reservation SET expire_time = NOW() - INTERVAL 1 MINUTE WHERE reservation_no = ?", r.No); err != nil {
		t.Fatalf("改过期时间: %v", err)
	}
	// 模拟「扫描已选中它」之后 Renew 抢先续期
	if err := s.Renew(ctx, testUser, "req-renew-race"); err != nil {
		t.Fatalf("续期: %v", err)
	}
	if err := release(ctx, conn, r.No); !errors.Is(err, errLeaseRenewed) {
		t.Fatalf("续期后的单不该被回收，实际 err=%v", err)
	}
	var st string
	if err := conn.QueryRowCtx(ctx, &st, "SELECT status FROM afv_billing_reservation WHERE reservation_no = ?", r.No); err != nil {
		t.Fatalf("查状态: %v", err)
	}
	if st != statusHeld {
		t.Fatalf("应仍为 HELD，实际 %s", st)
	}
}
