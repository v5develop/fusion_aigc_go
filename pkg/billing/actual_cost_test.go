package billing

import (
	"context"
	"errors"
	"math/big"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

// 公平点数：max(1, ceil(usd × rate × markup × 100))，有理数运算，整数边界不被浮点误差推高一档。
func TestFairPoints(t *testing.T) {
	cases := []struct {
		usd, rate, markup string
		want              int64
	}{
		{"0.014536", "7.2", "1.3", 14}, // 事故单：1k medium 文生图，原扣 836
		{"0.01", "7.2", "1.25", 9},     // 恰好 9.000…：不能 ceil 成 10
		{"0.1", "7.2", "1.3", 94},      // 93.6 → 94
		{"0", "7.2", "1.3", 1},         // 成本 0 也至少 1 点
		{"0.0000001", "7.2", "1.3", 1}, // 极小成本 → 1
		{"1.2", "7", "1", 840},         // 整数
	}
	for _, c := range cases {
		if got := FairPoints(rat(c.usd), rat(c.rate), rat(c.markup)); got != c.want {
			t.Errorf("FairPoints(%s,%s,%s)=%d want %d", c.usd, c.rate, c.markup, got, c.want)
		}
	}
	if got := ActualPoints(836, rat("0.014536"), rat("7.2"), rat("1.3")); got != 14 {
		t.Fatalf("ActualPoints=%d", got)
	}
	if got := ActualPoints(836, rat("5"), rat("7.2"), rat("1.3")); got != 836 {
		t.Fatalf("超过冻结额要封顶在冻结额: %d", got)
	}
}

func TestParseDecimal(t *testing.T) {
	for _, s := range []string{"", "abc", "0", "-1"} {
		if _, ok := ParseDecimal(s); ok {
			t.Errorf("%q 应非法", s)
		}
	}
	if r, ok := ParseDecimal(" 7.20 "); !ok || r.Cmp(rat("7.2")) != 0 {
		t.Fatal("7.20")
	}
}

// 明细行分摊与 Java allocateLines 同口径：Σ确认 = total − refunded、Σ退款 = refunded、每行不超过底数。
func TestAllocateLines(t *testing.T) {
	conf, ref := allocateLines(836, 822, []int64{836})
	if conf[0] != 14 || ref[0] != 822 {
		t.Fatalf("单行: %v %v", conf, ref)
	}
	conf, ref = allocateLines(100, 33, []int64{50, 30, 20})
	var sc, sr int64
	for i := range conf {
		sc, sr = sc+conf[i], sr+ref[i]
		if ref[i] < 0 || conf[i] < 0 {
			t.Fatalf("负数: %v %v", conf, ref)
		}
	}
	if sc != 67 || sr != 33 {
		t.Fatalf("和不对: %v %v", conf, ref)
	}
	// 行和 ≠ 整单（折扣 / 手工改价）：先按占比把整单分到各行
	conf, ref = allocateLines(10, 10, []int64{6, 6})
	if conf[0]+conf[1] != 0 || ref[0]+ref[1] != 10 {
		t.Fatalf("整单退: %v %v", conf, ref)
	}
	if c, r := allocateLines(5, 0, nil); len(c) != 0 || len(r) != 0 {
		t.Fatal("没有行")
	}
}

const (
	testNo   = "BRtest0001"
	testBiz  = "7_canvas-cv_1"
	testIdem = "golife:7_canvas-cv_1"
	snapGPT  = `{"settlement":"PARTIAL","quantity":1,"variantCode":"canvas_image_gpt26","carrierModelId":60}`
)

var (
	resvCols    = []string{"id", "reservation_no", "user_id", "price_points", "biz_id", "status", "feature_code", "price_snapshot", "idempotency_key"}
	qStatus     = regexp.QuoteMeta("SELECT `status` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ?")
	qLoadHeld   = regexp.QuoteMeta("FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `status` = ?")
	qCarrier    = regexp.QuoteMeta("SELECT `settle_mode`, `cost_markup` FROM `afv_ai_model` WHERE `id` = ?")
	qKnob       = regexp.QuoteMeta("FROM afv_system_config WHERE config_key = ?")
	qLockResv   = regexp.QuoteMeta("FROM `afv_billing_reservation` WHERE `reservation_no` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE")
	qLockAcc    = regexp.QuoteMeta("FROM `afv_points_account` WHERE `user_id` = ? AND `deleted` = 0 LIMIT 1 FOR UPDATE")
	qFreezes    = regexp.QuoteMeta("pl.`change_type` = 'FREEZE'")
	qBatches    = regexp.QuoteMeta("FROM `afv_points_batch` WHERE `id` IN (?)")
	eConsume    = regexp.QuoteMeta("UPDATE `afv_points_batch` SET `frozen` = `frozen` - ? WHERE `id` = ?")
	eGiveBack   = regexp.QuoteMeta("UPDATE `afv_points_batch` SET `frozen` = `frozen` - ?, `remaining` = `remaining` + ? WHERE `id` = ?")
	eActive     = regexp.QuoteMeta("UPDATE `afv_points_batch` SET `status` = 'ACTIVE' WHERE `id` = ?")
	eExhausted  = regexp.QuoteMeta("UPDATE `afv_points_batch` SET `status` = 'EXHAUSTED'")
	eAccSettle  = regexp.QuoteMeta("UPDATE `afv_points_account` SET `frozen_balance` = `frozen_balance` - ?, `balance` = `balance` + ?, `total_consumed`")
	eLog        = regexp.QuoteMeta("INSERT INTO `afv_points_log`")
	eLogLine    = regexp.QuoteMeta("INSERT INTO `afv_points_log_line`")
	eResvSettle = regexp.QuoteMeta("UPDATE `afv_billing_reservation` SET `status` = ?, `confirmed_time` = NOW()")
	qLines      = regexp.QuoteMeta("SELECT `id`, `reserved_points` FROM `afv_billing_reservation_line` WHERE `reservation_no` = ?")
	eLine       = regexp.QuoteMeta("UPDATE `afv_billing_reservation_line` SET `confirmed_points` = ?, `refunded_points` = ?, `status` = ? WHERE `id` = ?")
)

func onService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	conn, mock := mockConn(t)
	return New(Conf{Mode: ModeOn}, conn), mock
}

// expectHeld 状态查询 HELD + 按 biz_id 捞回单子。
func expectHeld(mock sqlmock.Sqlmock, reserved int64, snap string) {
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	mock.ExpectQuery(qLoadHeld).WithArgs("LOCALLIFE", testBiz, "HELD").WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, reserved, testBiz, "HELD", "ecom_image_gen", snap, testIdem))
}

// expectSettleTx 结算事务：一个批次（id 11，冻结 reserved），确认 consume、退回 reserved-consume，一行明细。
func expectSettleTx(mock sqlmock.Sqlmock, reserved, consume int64) {
	back := reserved - consume
	mock.ExpectBegin()
	mock.ExpectQuery(qLockResv).WithArgs(testNo).WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, reserved, testBiz, "HELD", "ecom_image_gen", snapGPT, testIdem))
	mock.ExpectQuery(qLockAcc).WithArgs(7).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).
		AddRow(5, 7, 1000, reserved, 1))
	mock.ExpectQuery(qFreezes).WithArgs(testIdem, 7).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, reserved))
	mock.ExpectQuery(qBatches).WithArgs(11, 7).WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen"}).AddRow(11, 1000, reserved))
	mock.ExpectExec(eConsume).WithArgs(consume, 11).WillReturnResult(sqlmock.NewResult(0, 1))
	if back > 0 {
		mock.ExpectExec(eGiveBack).WithArgs(back, back, 11).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(eActive).WithArgs(11).WillReturnResult(sqlmock.NewResult(0, 1))
	} else {
		mock.ExpectExec(eExhausted).WithArgs(11).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectExec(eAccSettle).WithArgs(reserved, back, consume, back, 5).WillReturnResult(sqlmock.NewResult(0, 1))
	status, ck, rk := "CONFIRMED", "confirm:"+testNo, "refunded:"+testNo
	if back > 0 {
		status, ck, rk = "PARTIALLY_REFUNDED", "partial-consume:"+testNo, "partial-refund:"+testNo
	}
	any := sqlmock.AnyArg()
	mock.ExpectExec(eLog).WithArgs(any, 7, "CONSUME", -consume, 1000, reserved-consume, "LOCALLIFE", testBiz, ck, "SYSTEM", any, any).
		WillReturnResult(sqlmock.NewResult(100, 1))
	mock.ExpectExec(eLogLine).WillReturnResult(sqlmock.NewResult(0, 1))
	if back > 0 {
		mock.ExpectExec(eLog).WithArgs(any, 7, "REFUND", back, 1000+back, 0, "LOCALLIFE", testBiz, rk, "SYSTEM", any, any).
			WillReturnResult(sqlmock.NewResult(101, 1))
		mock.ExpectExec(eLogLine).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(eResvSettle).WithArgs(status, back, 1, "HELD").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qLines).WithArgs(testNo).WillReturnRows(sqlmock.NewRows([]string{"id", "reserved_points"}).AddRow(31, reserved))
	mock.ExpectExec(eLine).WithArgs(consume, back, status, 31).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func costOf(usd string, calls *int) CostFunc {
	return func(context.Context) (*big.Rat, bool) {
		*calls++
		if usd == "" {
			return nil, false
		}
		return rat(usd), true
	}
}

// 事故单：GPT 2.6 载体 ACTUAL_COST，冻结 836，实报 $0.014536 → 确认 14、退回 822，明细行同步 confirmed/refunded。
func TestSettleCostActualCost(t *testing.T) {
	s, mock := onService(t)
	expectHeld(mock, 836, snapGPT)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock.ExpectQuery(qKnob).WithArgs(KeyUSDCNYRate).WillReturnRows(sqlmock.NewRows([]string{"v"}))    // 没配 → 7.2
	mock.ExpectQuery(qKnob).WithArgs(KeyDefaultMarkup).WillReturnRows(sqlmock.NewRows([]string{"v"})) // 没配 → 1.3
	expectSettleTx(mock, 836, 14)
	var n int
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.014536", &n))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !st.ActualCost || st.Points != 14 || st.Reserved != 836 || st.Mode != SettleActualCost || st.Already {
		t.Fatalf("结算结果: %+v calls=%d", st, n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 运行时汇率与载体自己的加价倍率生效：$0.1 × 7.0 × 1.5 × 100 = 105。
func TestSettleCostUsesRuntimeRateAndModelMarkup(t *testing.T) {
	s, mock := onService(t)
	expectHeld(mock, 836, snapGPT)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", "1.5000"))
	mock.ExpectQuery(qKnob).WithArgs(KeyUSDCNYRate).WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("7.0"))
	expectSettleTx(mock, 836, 105)
	var n int
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.1", &n))
	if err != nil || st.Points != 105 {
		t.Fatalf("%+v %v", st, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 拿不到实报成本（含没有成本来源）：按冻结全额确认，状态 CONFIRMED，不退差额。
func TestSettleCostActualCostUnavailableFallsBackToFull(t *testing.T) {
	for _, withSource := range []bool{true, false} {
		s, mock := onService(t)
		expectHeld(mock, 836, snapGPT)
		mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
		expectSettleTx(mock, 836, 836)
		var n int
		var cost CostFunc
		if withSource {
			cost = costOf("", &n)
		}
		st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, cost)
		if err != nil {
			t.Fatal(err)
		}
		if st.ActualCost || st.Points != 836 || st.Mode != SettleActualCost {
			t.Fatalf("回退全额: %+v", st)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}

// FIXED 载体：不调成本来源，按实际产出量结算（与原 Settle 一致）。
func TestSettleCostFixedIgnoresCost(t *testing.T) {
	s, mock := onService(t)
	expectHeld(mock, 20, snapGPT)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("FIXED", nil))
	expectSettleTx(mock, 20, 20)
	var n int
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.0001", &n))
	if err != nil || n != 0 || st.ActualCost || st.Points != 20 || st.Mode != SettleFixed {
		t.Fatalf("%+v %v calls=%d", st, err, n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 老库（Java 迁移前，afv_ai_model 没有 settle_mode 列）：按 FIXED，不崩；10 分钟内不再探测。
func TestSettleCostOldDBWithoutColumn(t *testing.T) {
	s, mock := onService(t)
	expectHeld(mock, 20, snapGPT)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnError(&mysql.MySQLError{Number: 1054, Message: "Unknown column 'settle_mode' in 'field list'"})
	expectSettleTx(mock, 20, 20)
	var n int
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.0001", &n))
	if err != nil || n != 0 || st.Mode != SettleFixed {
		t.Fatalf("%+v %v", st, err)
	}
	// 第二次：不再查 afv_ai_model（缓存了「没有列」）
	expectHeld(mock, 20, snapGPT)
	expectSettleTx(mock, 20, 20)
	if _, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.0001", &n)); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 旧单（快照里没有 carrierModelId）：按快照档位码查档位默认模型。
func TestSettleCostLegacySnapshotLooksUpVariant(t *testing.T) {
	s, mock := onService(t)
	expectHeld(mock, 836, `{"settlement":"PARTIAL","quantity":1,"variantCode":"canvas_image_gpt26"}`)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(`default_model_id`, 0) FROM `afv_billing_offer_variant` WHERE `variant_code` = ?")).
		WithArgs("canvas_image_gpt26").WillReturnRows(sqlmock.NewRows([]string{"default_model_id"}).AddRow(60))
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock.ExpectQuery(qKnob).WithArgs(KeyUSDCNYRate).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	mock.ExpectQuery(qKnob).WithArgs(KeyDefaultMarkup).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	expectSettleTx(mock, 836, 14)
	var n int
	if st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.014536", &n)); err != nil || st.Points != 14 {
		t.Fatalf("%+v %v", st, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 已结算的单子：幂等返回，不调成本来源、不动账。
func TestSettleCostIdempotent(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("PARTIALLY_REFUNDED"))
	var n int
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.01", &n))
	if err != nil || !st.Already || n != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	// 并发：状态查询是 HELD，事务里加锁时已被别处结算 → 不重复动账
	expectHeld(mock, 836, snapGPT)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock.ExpectQuery(qKnob).WithArgs(KeyUSDCNYRate).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	mock.ExpectQuery(qKnob).WithArgs(KeyDefaultMarkup).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	mock.ExpectBegin()
	mock.ExpectQuery(qLockResv).WithArgs(testNo).WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, 836, testBiz, "PARTIALLY_REFUNDED", "ecom_image_gen", snapGPT, testIdem))
	mock.ExpectCommit()
	st, err = s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.014536", &n))
	if err != nil || !st.Already {
		t.Fatalf("%+v %v", st, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 计费关闭：什么都不查。
func TestSettleCostOff(t *testing.T) {
	conn, mock := mockConn(t)
	s := New(Conf{Mode: ModeOff}, conn)
	st, err := s.SettleCost(context.Background(), 7, "x", 1, nil)
	if err != nil || st.Points != 0 {
		t.Fatal(st, err)
	}
	var nilSvc *Service
	if _, err := nilSvc.SettleCost(context.Background(), 7, "x", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 报价标出结算方式；估价带 settle_mode / max_points。
func TestQuoteCarriesSettleMode(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectQuery(qOffer).WithArgs("canvas_image").WillReturnRows(sqlmock.NewRows(offerCols).AddRow("canvas_image", "canvas_image", "ecom_image_gen"))
	mock.ExpectQuery(qByGroup).WithArgs("canvas_image", "img.gpt-image-2.6-sunburst").
		WillReturnRows(sqlmock.NewRows(variantCols).AddRow("canvas_image_gpt26", "standard", 60, nil, nil, "PARTIAL"))
	expectRest(mock, "canvas_image_gpt26", 60, "1K", 836)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	q, err := s.Quote(context.Background(), Charge{UserID: 7, EntryCode: "canvas_image", ModelGroup: "img.gpt-image-2.6-sunburst", Count: 1, Resolution: "1K"})
	if err != nil {
		t.Fatal(err)
	}
	if q.SettleMode != SettleActualCost || q.CarrierModelID != 60 || q.PricePoints != 836 {
		t.Fatalf("%+v", q)
	}
	if !regexp.MustCompile(`"carrierModelId":60`).MatchString(q.Snapshot()) {
		t.Fatalf("快照要带载体: %s", q.Snapshot())
	}
	e := estimate(context.Background(), &fakeEstimator{mode: ModeOn, quote: q, balance: 1000}, Charge{UserID: 7})
	if e.SettleMode != SettleActualCost || e.MaxPoints != 836 || e.Points != 836 {
		t.Fatalf("%+v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---- 已确认单补退 ----

var (
	qOverKey    = regexp.QuoteMeta("SELECT COUNT(1) FROM `afv_points_log` WHERE `idempotency_key` = ?")
	qConsumed   = regexp.QuoteMeta("pl.`change_type` = ? AND pl.`idempotency_key` IN (?, ?)")
	qLockBatchE = regexp.QuoteMeta("SELECT `id`,`remaining`,`frozen`,`expire_time`, ") + ".*" +
		regexp.QuoteMeta("AS `live` FROM `afv_points_batch` WHERE `id` = ? AND `user_id` = ? FOR UPDATE")
)

func expectLockConfirmed(mock sqlmock.Sqlmock, status string) {
	mock.ExpectBegin()
	mock.ExpectQuery(qLockResv).WithArgs(testNo).WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, 836, testBiz, status, "ecom_image_gen", snapGPT, testIdem))
}

// 历史单：已确认 836，按实报应扣 14 → 退 822 到原批次，单子置 PARTIALLY_REFUNDED，明细行重新分摊。
func TestRefundOvercharge(t *testing.T) {
	s, mock := onService(t)
	expectLockConfirmed(mock, "CONFIRMED")
	mock.ExpectQuery(qOverKey).WithArgs(OverchargeRefundKeyPrefix + testNo).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(qConsumed).WithArgs(7, "CONSUME", "confirm:"+testNo, "partial-consume:"+testNo).
		WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 800).AddRow(12, 36))
	mock.ExpectQuery(qLockAcc).WithArgs(7).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).AddRow(5, 7, 100, 0, 1))
	mock.ExpectQuery(qLockBatchE).WithArgs(11, 7).WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen", "expire_time", "live"}).AddRow(11, 0, 0, nil, 1))
	mock.ExpectQuery(qLockBatchE).WithArgs(12, 7).WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen", "expire_time", "live"}).AddRow(12, 0, 0, nil, 1))
	// 都不过期：id 大的先退（12 退 36，11 退 786）
	eBack := regexp.QuoteMeta("UPDATE `afv_points_batch` SET `remaining` = `remaining` + ?, `status` = 'ACTIVE' WHERE `id` = ?")
	mock.ExpectExec(eBack).WithArgs(36, 12).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(eBack).WithArgs(786, 11).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_account` SET `balance` = `balance` + ?, `total_consumed` = `total_consumed` - ?")).
		WithArgs(822, 822, 822, 5).WillReturnResult(sqlmock.NewResult(0, 1))
	any := sqlmock.AnyArg()
	mock.ExpectExec(eLog).WithArgs(any, 7, "REFUND", 822, 922, 0, "LOCALLIFE", testBiz, OverchargeRefundKeyPrefix+testNo, "SYSTEM", any, any).
		WillReturnResult(sqlmock.NewResult(200, 1))
	mock.ExpectExec(eLogLine).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(eLogLine).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_billing_reservation` SET `status` = ?, `refunded_time` = NOW() WHERE `id` = ? AND `status` = ?")).
		WithArgs("PARTIALLY_REFUNDED", 1, "CONFIRMED").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qLines).WithArgs(testNo).WillReturnRows(sqlmock.NewRows([]string{"id", "reserved_points"}).AddRow(31, 836))
	mock.ExpectExec(eLine).WithArgs(14, 822, "PARTIALLY_REFUNDED", 31).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := s.RefundOvercharge(context.Background(), testNo, 14, "")
	if err != nil || got != 822 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 幂等：补退流水已存在 → 0，不动账；并发下插流水撞唯一键 → 0。
func TestRefundOverchargeIdempotent(t *testing.T) {
	s, mock := onService(t)
	expectLockConfirmed(mock, "PARTIALLY_REFUNDED")
	mock.ExpectQuery(qOverKey).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectCommit()
	if got, err := s.RefundOvercharge(context.Background(), testNo, 14, ""); err != nil || got != 0 {
		t.Fatalf("got=%d err=%v", got, err)
	}

	expectLockConfirmed(mock, "CONFIRMED")
	mock.ExpectQuery(qOverKey).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(qConsumed).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 836))
	mock.ExpectQuery(qLockAcc).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).AddRow(5, 7, 100, 0, 1))
	mock.ExpectQuery(qLockBatchE).WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen", "expire_time", "live"}).AddRow(11, 0, 0, nil, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_batch` SET `remaining`")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_account`")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(eLog).WillReturnError(&mysql.MySQLError{Number: 1062, Message: "Duplicate entry"})
	mock.ExpectRollback()
	if got, err := s.RefundOvercharge(context.Background(), testNo, 14, ""); err != nil || got != 0 {
		t.Fatalf("撞键: got=%d err=%v", got, err)
	}

	// 实扣已不高于应扣：不退
	expectLockConfirmed(mock, "CONFIRMED")
	mock.ExpectQuery(qOverKey).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(qConsumed).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 14))
	mock.ExpectCommit()
	if got, err := s.RefundOvercharge(context.Background(), testNo, 14, ""); err != nil || got != 0 {
		t.Fatalf("不用退: got=%d err=%v", got, err)
	}

	// 不是已确认的单：拒绝
	expectLockConfirmed(mock, "REFUNDED")
	mock.ExpectRollback()
	if _, err := s.RefundOvercharge(context.Background(), testNo, 14, ""); err == nil {
		t.Fatal("REFUNDED 的单不能补退")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// PARTIAL 档位多张部分成功：按实际用量结算的上限是按交付张数本该确认的点数（与 Java chargedAfterUndelivered 同口径），
// 不能因为失败但被供应商计费的尝试比固定价扣得还多。冻结 400（4 张），交付 1 张 → 上限 100；实报 $0.5 → 468 → 确认 100。
func TestSettleCostActualCostCappedByDelivered(t *testing.T) {
	s, mock := onService(t)
	snap4 := `{"settlement":"PARTIAL","quantity":4,"variantCode":"canvas_image_gpt26","carrierModelId":60}`
	expectHeld(mock, 400, snap4)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock.ExpectQuery(qKnob).WithArgs(KeyUSDCNYRate).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	mock.ExpectQuery(qKnob).WithArgs(KeyDefaultMarkup).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	expectSettleTx(mock, 400, 100)
	var n int
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, costOf("0.5", &n))
	if err != nil || !st.ActualCost || st.Points != 100 {
		t.Fatalf("%+v %v", st, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 补退只碰 Go 冻结的单（golife: 幂等键）；Java 的单归 Java 的历史补退，碰了两边幂等键不同会重复退。
func TestRefundOverchargeRejectsJavaReservation(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectBegin()
	mock.ExpectQuery(qLockResv).WithArgs(testNo).WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, 836, testBiz, "CONFIRMED", "ecom_image_gen", snapGPT, "creation:abc"))
	mock.ExpectRollback()
	if got, err := s.RefundOvercharge(context.Background(), testNo, 14, ""); err == nil || got != 0 {
		t.Fatalf("Java 的单应拒绝: got=%d err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 已按实际用量补退过 822 的单（PARTIALLY_REFUNDED）再人工整单退回：只退剩下的 14，不能把 836 再退一遍；明细行整单记退款。
func TestRefundConfirmedAfterOvercharge(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `reservation_no` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ?")).
		WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"reservation_no"}).AddRow(testNo))
	mock.ExpectQuery(qLockResv).WithArgs(testNo).WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, 836, testBiz, "PARTIALLY_REFUNDED", "ecom_image_gen", snapGPT, testIdem))
	mock.ExpectQuery(qConsumed).WithArgs(7, "CONSUME", "confirm:"+testNo, "partial-consume:"+testNo).
		WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 836))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT ll.`batch_id`, ll.`amount` FROM `afv_points_log` pl")).
		WithArgs(7, OverchargeRefundKeyPrefix+testNo).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 822))
	mock.ExpectQuery(qLockAcc).WithArgs(7).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).AddRow(5, 7, 922, 0, 1))
	mock.ExpectQuery(qLockBatchE).WithArgs(11, 7).
		WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen", "expire_time", "live"}).AddRow(11, 822, 0, nil, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_batch` SET `remaining` = `remaining` + ?, `status` = 'ACTIVE' WHERE `id` = ?")).
		WithArgs(14, 11).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_account` SET `balance` = `balance` + ?, `total_consumed` = `total_consumed` - ?")).
		WithArgs(14, 14, 14, 5).WillReturnResult(sqlmock.NewResult(0, 1))
	any := sqlmock.AnyArg()
	mock.ExpectExec(eLog).WithArgs(any, 7, "REFUND", 14, 936, 0, "LOCALLIFE", testBiz, "post-refund:"+testNo, "SYSTEM", any, any).
		WillReturnResult(sqlmock.NewResult(300, 1))
	mock.ExpectExec(eLogLine).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_billing_reservation` SET `status` = ?, `refunded_time` = NOW() WHERE `id` = ? AND `status` = ?")).
		WithArgs("REFUNDED", 1, "PARTIALLY_REFUNDED").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qLines).WithArgs(testNo).WillReturnRows(sqlmock.NewRows([]string{"id", "reserved_points"}).AddRow(31, 836))
	mock.ExpectExec(eLine).WithArgs(0, 836, "REFUNDED", 31).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := s.RefundConfirmed(context.Background(), 7, "canvas-cv_1", "人工")
	if err != nil || got != 14 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 原批次有的已过期：只退仍可用的批次（不复活过期批次），过期批次那部分落 REFUND 新批次（永不过期，与 Java 一致）；
// 流水一条 REFUND、明细两行（原批次 + 新批次），和 = 补退额。
func TestRefundOverchargeExpiredBatchGoesToNewRefundBatch(t *testing.T) {
	s, mock := onService(t)
	expectLockConfirmed(mock, "CONFIRMED")
	mock.ExpectQuery(qOverKey).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(qConsumed).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 800).AddRow(12, 36))
	mock.ExpectQuery(qLockAcc).WithArgs(7).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).AddRow(5, 7, 100, 0, 1))
	past, future := time.Now().Add(-24*time.Hour), time.Now().Add(30*24*time.Hour)
	cols := []string{"id", "remaining", "frozen", "expire_time", "live"}
	mock.ExpectQuery(qLockBatchE).WithArgs(11, 7).WillReturnRows(sqlmock.NewRows(cols).AddRow(11, 0, 0, past, 0))   // 已过期
	mock.ExpectQuery(qLockBatchE).WithArgs(12, 7).WillReturnRows(sqlmock.NewRows(cols).AddRow(12, 5, 0, future, 1)) // 仍可用
	eBack := regexp.QuoteMeta("UPDATE `afv_points_batch` SET `remaining` = `remaining` + ?, `status` = 'ACTIVE' WHERE `id` = ?")
	mock.ExpectExec(eBack).WithArgs(36, 12).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_points_batch`")).
		WithArgs(5, 7, 786, 786, "REFUND", testNo).WillReturnResult(sqlmock.NewResult(99, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_account` SET `balance` = `balance` + ?, `total_consumed` = `total_consumed` - ?")).
		WithArgs(822, 822, 822, 5).WillReturnResult(sqlmock.NewResult(0, 1))
	any := sqlmock.AnyArg()
	mock.ExpectExec(eLog).WithArgs(any, 7, "REFUND", 822, 922, 0, "LOCALLIFE", testBiz, OverchargeRefundKeyPrefix+testNo, "SYSTEM", any, any).
		WillReturnResult(sqlmock.NewResult(200, 1))
	mock.ExpectExec(eLogLine).WithArgs(200, 12, 7, 36, 5, 41, 0, 0).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(eLogLine).WithArgs(200, 99, 7, 786, 0, 786, 0, 0).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_billing_reservation` SET `status` = ?, `refunded_time` = NOW() WHERE `id` = ? AND `status` = ?")).
		WithArgs("PARTIALLY_REFUNDED", 1, "CONFIRMED").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qLines).WithArgs(testNo).WillReturnRows(sqlmock.NewRows([]string{"id", "reserved_points"}).AddRow(31, 836))
	mock.ExpectExec(eLine).WithArgs(14, 822, "PARTIALLY_REFUNDED", 31).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := s.RefundOvercharge(context.Background(), testNo, 14, "")
	if err != nil || got != 822 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 补退时 786 落进了 REFUND 新批次（原批次 11 已过期）、36 退回批次 12；再人工整单退回只退剩下的 14，
// 不能因为新批次对不上原批次就把 786 再退一遍；批次 11 仍过期 → 这 14 也落新批次。
func TestRefundConfirmedAfterOverchargeIntoNewBatch(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `reservation_no` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ?")).
		WillReturnRows(sqlmock.NewRows([]string{"reservation_no"}).AddRow(testNo))
	mock.ExpectQuery(qLockResv).WithArgs(testNo).WillReturnRows(sqlmock.NewRows(resvCols).
		AddRow(1, testNo, 7, 836, testBiz, "PARTIALLY_REFUNDED", "ecom_image_gen", snapGPT, testIdem))
	mock.ExpectQuery(qConsumed).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, 800).AddRow(12, 36))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT ll.`batch_id`, ll.`amount` FROM `afv_points_log` pl")).
		WithArgs(7, OverchargeRefundKeyPrefix+testNo).WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(12, 36).AddRow(99, 786))
	mock.ExpectQuery(qLockAcc).WithArgs(7).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).AddRow(5, 7, 922, 0, 1))
	mock.ExpectQuery(qLockBatchE).WithArgs(11, 7).WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen", "expire_time", "live"}).
		AddRow(11, 0, 0, time.Now().Add(-time.Hour), 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_points_batch`")).WithArgs(5, 7, 14, 14, "REFUND", testNo).WillReturnResult(sqlmock.NewResult(100, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_points_account` SET `balance` = `balance` + ?")).WithArgs(14, 14, 14, 5).WillReturnResult(sqlmock.NewResult(0, 1))
	any := sqlmock.AnyArg()
	mock.ExpectExec(eLog).WithArgs(any, 7, "REFUND", 14, 936, 0, "LOCALLIFE", testBiz, "post-refund:"+testNo, "SYSTEM", any, any).
		WillReturnResult(sqlmock.NewResult(300, 1))
	mock.ExpectExec(eLogLine).WithArgs(300, 100, 7, 14, 0, 14, 0, 0).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_billing_reservation` SET `status` = ?")).WithArgs("REFUNDED", 1, "PARTIALLY_REFUNDED").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qLines).WillReturnRows(sqlmock.NewRows([]string{"id", "reserved_points"}).AddRow(31, 836))
	mock.ExpectExec(eLine).WithArgs(0, 836, "REFUNDED", 31).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := s.RefundConfirmed(context.Background(), 7, "canvas-cv_1", "人工")
	if err != nil || got != 14 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 并发：状态查询是 HELD，捞单时已被另一次确认扣除 → 重读到 CONFIRMED，幂等成功，不误报 ErrGone。
func TestSettleCostConcurrentConfirmIsAlready(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	mock.ExpectQuery(qLoadHeld).WithArgs("LOCALLIFE", testBiz, "HELD").WillReturnRows(sqlmock.NewRows(resvCols))
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("CONFIRMED"))
	st, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, nil)
	if err != nil || !st.Already {
		t.Fatalf("并发确认应幂等成功：%+v %v", st, err)
	}
	// 捞单时已被回收 → 重读 EXPIRED → ErrGone
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	mock.ExpectQuery(qLoadHeld).WithArgs("LOCALLIFE", testBiz, "HELD").WillReturnRows(sqlmock.NewRows(resvCols))
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("EXPIRED"))
	if _, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, nil); !errors.Is(err, ErrGone) {
		t.Fatalf("被回收应 ErrGone：%v", err)
	}
	// 重读失败：保持原口径 ErrGone（不交付）
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	mock.ExpectQuery(qLoadHeld).WithArgs("LOCALLIFE", testBiz, "HELD").WillReturnRows(sqlmock.NewRows(resvCols))
	mock.ExpectQuery(qStatus).WithArgs("LOCALLIFE", testBiz).WillReturnError(errors.New("conn reset"))
	if _, err := s.SettleCost(context.Background(), 7, "canvas-cv_1", 1, nil); !errors.Is(err, ErrGone) {
		t.Fatalf("重读失败应 ErrGone：%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
