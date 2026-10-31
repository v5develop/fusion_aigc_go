package logic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/billing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ---------------- Seedream 计费档位：受理阶段归一，冻结 / 快照 / gateway 请求都用 2K ----------------

func seedreamReq(size string) *canvas.RunNodeRequest {
	return &canvas.RunNodeRequest{Kind: "image", Model: "SD5P", Prompt: "白底棚拍", Ratio: "16:9", Size: size, Count: 1}
}

// billingQuoteMock 计费库 sqlmock：offer / 档位 / 步骤 / 规则照常返回，单价行按分辨率参数区分（2K=20、4K=40），
// 用来断言 Charge 真正带进计价 SQL 的是哪个档位。
func billingQuoteMock(t *testing.T, wantRes string, points int64) *billing.Service {
	t.Helper()
	sdb, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	mock.MatchExpectationsInOrder(true)
	mock.ExpectQuery("FROM `afv_billing_offer`").WithArgs("canvas_image").
		WillReturnRows(sqlmock.NewRows([]string{"offer_code", "entry_code", "feature_code"}).AddRow("canvas_image", "canvas_image", "ecom_image_gen"))
	mock.ExpectQuery("FROM `afv_billing_offer_variant`").WithArgs("canvas_image").
		WillReturnRows(sqlmock.NewRows([]string{"variant_code", "quality_tier", "default_model_id", "default_resolution", "default_duration_seconds", "settlement_type"}).
			AddRow("canvas_image_std", nil, 61, "1K", nil, "PER_UNIT"))
	mock.ExpectQuery("FROM `afv_billing_charge_component`").WithArgs("canvas_image_std").
		WillReturnRows(sqlmock.NewRows([]string{"component_code", "step_code", "component_type", "mode", "cost_type", "action_type", "model_id", "base_points", "price_multiplier"}).
			AddRow("gen", nil, "IMAGE", nil, "BILLED", "IMAGE_GEN", nil, nil, nil))
	mock.ExpectQuery("FROM `afv_billing_charge_rule`").WillReturnRows(sqlmock.NewRows([]string{"rule_code", "scope_type", "scope_code", "factor_key"}))
	mock.ExpectQuery("FROM `afv_model_pricing`").WithArgs(61, "IMAGE_GEN", wantRes).
		WillReturnRows(sqlmock.NewRows([]string{"id", "price_points", "price_unit", "resolution"}).AddRow(9, points, "PER_IMAGE", wantRes))
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("计价 SQL 没按预期走: %v", err)
		}
	})
	return billing.New(billing.Conf{Mode: billing.ModeOn}, sqlx.NewSqlConnFromDB(sdb))
}

func TestSeedream4KFreezesAs2K(t *testing.T) {
	c := testCore()
	req := seedreamReq("4K")

	// 受理：请求先归一（写回 req.Size，request 快照存 2K），回告警
	warns := c.normalizeRunSize(req)
	if req.Size != "2K" || len(warns) != 1 || !strings.Contains(warns[0], "4K") {
		t.Fatalf("Seedream 4K 应归一为 2K 并告警: size=%s warns=%v", req.Size, warns)
	}
	if b, _ := json.Marshal(toStored(req)); !strings.Contains(string(b), `"size":"2K"`) {
		t.Fatalf("run.request 快照应存归一值: %s", b)
	}
	// 同一个原始请求再归一一次结果相同（幂等重放比对的是归一后的快照）
	again := seedreamReq("4K")
	_ = c.normalizeRunSize(again)
	a, _ := json.Marshal(toStored(req))
	b, _ := json.Marshal(toStored(again))
	if !sameRequest(a, b) {
		t.Fatal("同一请求两次归一的快照应相同")
	}

	// buildSpec 直接吃未归一的 4K（轮询器从旧快照重建 / 其它入口）也得到 2K：目录只给 2K 也不会报「不支持分辨率 4K」
	sp, err := c.buildSpec(seedreamReq("4K"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Size != "2K" || len(sp.Warnings) != 1 {
		t.Fatalf("spec 应为 2K 并带告警: %+v", sp)
	}
	ch := c.charge(7, "canvas-cv_t", sp)
	if ch.Resolution != "2K" || ch.EntryCode != "canvas_image" || ch.Count != 1 {
		t.Fatalf("冻结 Charge 应按 2K: %+v", ch)
	}
	// 冻结金额 = 2K 单价：计价 SQL 收到的分辨率参数是 2K（mock 只认 2K，带 4K 会不匹配报错）
	c.s.Billing = billingQuoteMock(t, "2K", 20)
	q, err := c.s.Billing.Quote(context.Background(), ch)
	if err != nil || q.PricePoints != 20 {
		t.Fatalf("应按 2K 单价 20 计价: %+v %v", q, err)
	}

	// gateway 请求同样是 2K
	run := &model.NodeRun{ID: 9, Kind: model.KindImage, CanvasID: "cv_x", UserID: 7, TaskID: sql.NullString{String: "cv_t", Valid: true}}
	reqs, err := c.gatewayRequests(context.Background(), run, sp)
	if err != nil {
		t.Fatal(err)
	}
	if in, _ := json.Marshal(reqs[0].Input); !strings.Contains(string(in), `"resolution":"2K"`) {
		t.Fatalf("gateway 请求应为 2K: %s", in)
	}
}

func TestNormalizeImageSize(t *testing.T) {
	cases := []struct {
		group, size, want string
		warn              bool
	}{
		{"img.seedream-5-pro", "4K", "2K", true},
		{"img.seedream-5-pro", "4k", "2K", true},
		{"img.seedream-5-pro", "1K", "2K", true},
		{"img.seedream-5-pro", "2K", "2K", false},
		{"img.seedream-5-pro", "", "2K", false},
		{"img.gpt-image-2", "4K", "4K", false},
		{"img.nano-banana-2.1", "1K", "1K", false},
	}
	for _, x := range cases {
		got, w := normalizeImageSize(x.group, x.size)
		if got != x.want || (len(w) > 0) != x.warn {
			t.Errorf("%s %s → %s %v，want %s warn=%v", x.group, x.size, got, w, x.want, x.warn)
		}
	}
	// 非 Seedream / 非 image 的请求不动
	c := testCore()
	g := &canvas.RunNodeRequest{Kind: "image", Model: "GPT", Prompt: "p", Size: "4K"}
	v := &canvas.RunNodeRequest{Kind: "video", Model: "Seedance 2.0", Prompt: "p", Size: "720p"}
	if c.normalizeRunSize(g) != nil || g.Size != "4K" || c.normalizeRunSize(v) != nil || v.Size != "720p" {
		t.Fatal("非 Seedream 请求不应改动")
	}
}

// ---------------- UNKNOWN 计费镜像回写（run_guard.reconcileBilling） ----------------

func TestRunBillingMirror(t *testing.T) {
	want := map[string]string{
		"CONFIRMED": model.BillSettled, "PARTIALLY_REFUNDED": model.BillSettled,
		"REFUNDED": model.BillRefunded, "EXPIRED": model.BillRefunded, "CANCELLED": model.BillRefunded,
	}
	for st, to := range want {
		if got, ok := runBillingMirror(st); !ok || got != to {
			t.Errorf("%s → %s %v，want %s", st, got, ok, to)
		}
	}
	for _, st := range []string{"HELD", "", "WHATEVER"} {
		if _, ok := runBillingMirror(st); ok {
			t.Errorf("%q 不应回写", st)
		}
	}
}

const qResvStatus = "SELECT `status` FROM `afv_billing_reservation` WHERE `biz_type` = \\? AND `biz_id` = \\? AND `deleted` = 0 LIMIT 1"

func TestReconcileBillingMirror(t *testing.T) {
	c, mock := mockCore(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	bdb, bmock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bdb.Close() })
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOn}, sqlx.NewSqlConnFromDB(bdb))

	long := "canvas-" + strings.Repeat("x", 70) // 超 64 字节：biz_id 按 pkg/billing 规则折叠
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.status IN (2, 3, 4)) AND (r.updated_at >= '2026-10-24 12:00:00") +
		".*" + regexp.QuoteMeta("AND (r.backend = 'gateway') AND (r.billing_status IN ('review', 'held', 'unsettled'))") +
		".*" + regexp.QuoteMeta("AND (r.id > 0) ORDER BY r.id ASC LIMIT 500")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "billing_request_id", "billing_status"}).
			AddRow(1, 7, "canvas-cv_1", "review"). // 到期被 Reclaimer 释放 → refunded
			AddRow(2, 7, "canvas-cv_2", "review"). // 运营人工确认扣费 → settled
			AddRow(3, 7, "canvas-cv_3", "held").   // 退款释放 → refunded
			AddRow(4, 7, "canvas-cv_4", "review"). // 仍 HELD（回收器还没到期）→ 不动
			AddRow(5, 7, long, "review"))          // 无单 → 不动
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_1").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("EXPIRED"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET billing_status = 'refunded' WHERE (id = 1) AND (billing_status = 'review')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_2").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("CONFIRMED"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET billing_status = 'settled' WHERE (id = 2) AND (billing_status = 'review')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_3").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("REFUNDED"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET billing_status = 'refunded' WHERE (id = 3) AND (billing_status = 'held')")).
		WillReturnResult(sqlmock.NewResult(0, 0)) // CAS 没抢到（并发路径已写）：不报错
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_4").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	folded := billingFold(7, long)
	if len(folded) != 64 {
		t.Fatalf("折叠后应为 64 字节: %d", len(folded))
	}
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", folded).WillReturnRows(sqlmock.NewRows([]string{"status"}))

	c.reconcileBilling(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := bmock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func reconcileCore(t *testing.T) (*core, sqlmock.Sqlmock, sqlmock.Sqlmock) {
	c, mock := mockCore(t)
	bdb, bmock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bdb.Close() })
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOn}, sqlx.NewSqlConnFromDB(bdb))
	return c, mock, bmock
}

var reconcileCols = []string{"id", "user_id", "kind", "billing_request_id", "billing_status", "candidates"}

// unsettled（已交付、确认扣费暂时失败）：重试 Settle。成功 → settled；ErrGone（已释放 / 并发改了状态）→ 保持 unsettled、
// 只 ERROR 告警一次；其它错误 → 不动、下轮再试。
func TestReconcileUnsettledRetriesSettle(t *testing.T) {
	c, mock, bmock := reconcileCore(t)
	mock.ExpectQuery(regexp.QuoteMeta("r.billing_status IN ('review', 'held', 'unsettled')")).
		WillReturnRows(sqlmock.NewRows(reconcileCols).
			AddRow(1, 7, "image", "canvas-cv_1", "unsettled", `[{"rid":"a"},{"rid":"b"}]`). // 已 CONFIRMED（幂等）→ settled
			AddRow(2, 7, "image", "canvas-cv_2", "unsettled", nil).                         // 已被回收释放 → ErrGone，保持
			AddRow(3, 7, "video", "canvas-cv_3", "unsettled", nil).                         // HELD 但加锁时已被改 → ErrGone，保持
			AddRow(4, 7, "image", "canvas-cv_4", "unsettled", nil))                         // 查库出错 → 暂时性，保持
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_1").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("CONFIRMED"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET billing_status = 'settled' WHERE (id = 1) AND (billing_status = 'unsettled')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_2").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("EXPIRED"))
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_3").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
	bmock.ExpectQuery("SELECT .* FROM `afv_billing_reservation` WHERE `biz_type` = \\? AND `biz_id` = \\? AND `status` = \\?").
		WithArgs("LOCALLIFE", "7_canvas-cv_3", "HELD").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	bmock.ExpectQuery(qResvStatus).WithArgs("LOCALLIFE", "7_canvas-cv_4").WillReturnError(errors.New("conn reset"))
	c.reconcileBilling(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := bmock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if !c.unsettledAlerted[2] || !c.unsettledAlerted[3] || c.unsettledAlerted[4] || c.unsettledAlerted[1] {
		t.Fatalf("ErrGone 的才告警: %v", c.unsettledAlerted)
	}
	if c.reconcileAfter != 0 {
		t.Fatalf("没取满一页，游标应回到开头: %d", c.reconcileAfter)
	}
}

// 积压超过一页：按 id 游标轮转，下一轮从上页末尾接着取，取不满回到开头（不被长期 HELD 的行饿死）。
func TestReconcileCursorRotates(t *testing.T) {
	c, mock, bmock := reconcileCore(t)
	old := reconcileLimit
	reconcileLimit = 2
	t.Cleanup(func() { reconcileLimit = old })
	held := func(ids ...int64) *sqlmock.Rows {
		rows := sqlmock.NewRows(reconcileCols)
		for _, id := range ids {
			rows.AddRow(id, 7, "image", fmt.Sprintf("canvas-cv_%d", id), "review", nil)
			bmock.ExpectQuery(qResvStatus).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("HELD"))
		}
		return rows
	}
	mock.ExpectQuery(regexp.QuoteMeta("AND (r.id > 0) ORDER BY r.id ASC LIMIT 2")).WillReturnRows(held(1, 2))
	c.reconcileBilling(context.Background())
	mock.ExpectQuery(regexp.QuoteMeta("AND (r.id > 2) ORDER BY r.id ASC LIMIT 2")).WillReturnRows(held(5))
	c.reconcileBilling(context.Background())
	mock.ExpectQuery(regexp.QuoteMeta("AND (r.id > 0) ORDER BY r.id ASC LIMIT 2")).WillReturnRows(held())
	c.reconcileBilling(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := bmock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 计费未开启：不查、不写。
func TestReconcileBillingOff(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.reconcileBilling(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// billingFold biz_id 折叠规则（同 pkg/billing Charge.bizID / tests/e2e/lib/billing.sh 的 SQL 版本）：
// {user_id}_{request_id}，超过 64 字节折叠成 {user_id}_{SHA-256(原串) 十六进制} 截前 64 位。
func billingFold(userID int64, requestID string) string {
	id := fmt.Sprintf("%d_%s", userID, requestID)
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%d_%x", userID, sum[:])[:64]
}
