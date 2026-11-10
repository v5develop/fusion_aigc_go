package logic

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"

	"github.com/DATA-DOG/go-sqlmock"
)

const snapGPT26 = `{"settlement":"PARTIAL","quantity":1,"variantCode":"canvas_image_gpt26","carrierModelId":60}`

var resvCols = []string{"id", "reservation_no", "user_id", "price_points", "biz_id", "status", "feature_code", "price_snapshot", "idempotency_key"}

// expectCanvasSettle 计费库：HELD 单（冻结 836，载体 60 = ACTUAL_COST）→ 结算事务确认 consume。
func expectCanvasSettle(bmock sqlmock.Sqlmock, consume int64, withKnobs bool) {
	const reserved, no, biz, idem = int64(836), "BR1", "7_canvas-cv_1", "golife:7_canvas-cv_1"
	back := reserved - consume
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", biz).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	bmock.ExpectQuery("FROM `afv_billing_reservation` WHERE `biz_type` = \\? AND `biz_id` = \\? AND `status` = \\?").
		WillReturnRows(sqlmock.NewRows(resvCols).AddRow(1, no, 7, reserved, biz, "HELD", "ecom_image_gen", snapGPT26, idem))
	bmock.ExpectQuery("SELECT `settle_mode`, `cost_markup` FROM `afv_ai_model`").WithArgs(60).
		WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	if withKnobs {
		bmock.ExpectQuery("FROM afv_system_config").WillReturnRows(sqlmock.NewRows([]string{"v"}))
		bmock.ExpectQuery("FROM afv_system_config").WillReturnRows(sqlmock.NewRows([]string{"v"}))
	}
	bmock.ExpectBegin()
	bmock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(resvCols).AddRow(1, no, 7, reserved, biz, "HELD", "ecom_image_gen", snapGPT26, idem))
	bmock.ExpectQuery("FROM `afv_points_account`").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "balance", "frozen_balance", "version"}).AddRow(5, 7, 0, reserved, 1))
	bmock.ExpectQuery("'FREEZE'").WillReturnRows(sqlmock.NewRows([]string{"batch_id", "amount"}).AddRow(11, reserved))
	bmock.ExpectQuery("FROM `afv_points_batch`").WillReturnRows(sqlmock.NewRows([]string{"id", "remaining", "frozen"}).AddRow(11, 0, reserved))
	bmock.ExpectExec("UPDATE `afv_points_batch`").WithArgs(consume, 11).WillReturnResult(sqlmock.NewResult(0, 1))
	if back > 0 {
		bmock.ExpectExec("UPDATE `afv_points_batch`").WithArgs(back, back, 11).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	bmock.ExpectExec("UPDATE `afv_points_batch` SET `status`").WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectExec("UPDATE `afv_points_account`").WithArgs(reserved, back, consume, back, 5).WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectExec("INSERT INTO `afv_points_log`").WillReturnResult(sqlmock.NewResult(100, 1))
	bmock.ExpectExec("INSERT INTO `afv_points_log_line`").WillReturnResult(sqlmock.NewResult(0, 1))
	if back > 0 {
		bmock.ExpectExec("INSERT INTO `afv_points_log`").WillReturnResult(sqlmock.NewResult(101, 1))
		bmock.ExpectExec("INSERT INTO `afv_points_log_line`").WillReturnResult(sqlmock.NewResult(0, 1))
	}
	bmock.ExpectExec("UPDATE `afv_billing_reservation` SET").WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectQuery("FROM `afv_billing_reservation_line`").WillReturnRows(sqlmock.NewRows([]string{"id", "reserved_points"}).AddRow(31, reserved))
	bmock.ExpectExec("UPDATE `afv_billing_reservation_line`").WithArgs(consume, back, sqlmock.AnyArg(), 31).WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectCommit()
}

func gwRun() *model.NodeRun {
	return &model.NodeRun{ID: 1, UserID: 7, Kind: model.KindImage, Points: 836, BillingStatus: model.BillHeld,
		BillingRequestID: sql.NullString{String: "canvas-cv_1", Valid: true}, BackendRef: sql.NullString{String: "call_a", Valid: true}}
}

// 画布 GPT 2.6 run：本轮判定的调用带实报成本 → 确认 14、退 822，run.points 改成 14、billing_status=settled。
func TestCanvasSettleActualCost(t *testing.T) {
	c, mock, bmock := reconcileCore(t)
	expectCanvasSettle(bmock, 14, true)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` AS `r` SET `billing_status` = 'settled', `points` = 14 WHERE (id = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	run := gwRun()
	calls := []*aigateway.Call{{ID: "call_a", Status: aigateway.StatusSucceeded,
		Cost: &aigateway.Cost{Amount: "0.014536", Currency: "USD", Source: aigateway.CostSourceReported, Final: true}}}
	c.settle(context.Background(), run, 1, calls)
	if run.Points != 14 || run.BillingStatus != model.BillSettled {
		t.Fatalf("points=%d billing=%s", run.Points, run.BillingStatus)
	}
	if err := bmock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 拿不到实报成本（调用成本未定稿、gateway 未接入无法重查）：按冻结全额确认，points 不改。
func TestCanvasSettleActualCostUnavailable(t *testing.T) {
	c, mock, bmock := reconcileCore(t)
	expectCanvasSettle(bmock, 836, false)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` AS `r` SET `billing_status` = 'settled' WHERE (id = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	run := gwRun()
	calls := []*aigateway.Call{{ID: "call_a", Status: aigateway.StatusSucceeded,
		Cost: &aigateway.Cost{Amount: "0.02", Currency: "USD", Source: aigateway.CostSourceEstimated}}}
	c.settle(context.Background(), run, 1, calls)
	if run.Points != 836 || run.BillingStatus != model.BillSettled {
		t.Fatalf("points=%d billing=%s", run.Points, run.BillingStatus)
	}
	if err := bmock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 一个 run 多个调用（count>1 拆多个调用）：成本求和；缺一个调用 → 不可用。
func TestCanvasRunCostSumsCalls(t *testing.T) {
	c, _ := mockCore(t)
	run := gwRun()
	run.BackendRef = sql.NullString{String: "call_a,call_b", Valid: true}
	rep := func(id, usd string) *aigateway.Call {
		return &aigateway.Call{ID: id, Status: aigateway.StatusSucceeded,
			Cost: &aigateway.Cost{Amount: usd, Currency: "USD", Source: aigateway.CostSourceReported, Final: true}}
	}
	if v, ok := c.runCost(run, []*aigateway.Call{rep("call_a", "0.01"), rep("call_b", "0.02")})(context.Background()); !ok || v.FloatString(2) != "0.03" {
		t.Fatalf("%v %v", v, ok)
	}
	// 只拿到一个、gateway 未接入无法补查 → 不可用
	if _, ok := c.runCost(run, []*aigateway.Call{rep("call_a", "0.01")})(context.Background()); ok {
		t.Fatal("缺调用应不可用")
	}
}

// 调用级成本未定稿、但每个扣费尝试都已实报：gateway 补查不了时仍用手上这批调用按尝试合计，不白白退化成全额确认。
func TestCanvasRunCostFallsBackToAttemptsWhenGatewayOff(t *testing.T) {
	c, _ := mockCore(t)
	run := gwRun()
	call := &aigateway.Call{ID: "call_a", Status: aigateway.StatusSucceeded,
		Cost: &aigateway.Cost{Amount: "0.05", Currency: "USD", Source: aigateway.CostSourceEstimated},
		Attempts: []aigateway.Attempt{
			{Charged: aigateway.ChargedNo},
			{Charged: aigateway.ChargedYes, Cost: &aigateway.Cost{Amount: "0.014536", Currency: "USD", Source: aigateway.CostSourceReported, Final: true}},
		}}
	if v, ok := c.runCost(run, []*aigateway.Call{call})(context.Background()); !ok || v.FloatString(6) != "0.014536" {
		t.Fatalf("%v %v", v, ok)
	}
}
