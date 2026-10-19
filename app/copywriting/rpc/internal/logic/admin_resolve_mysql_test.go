package logic

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/config"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 与其他集成测试共用同一个库，请用 go test -p 1 串行运行。
// 真实 MySQL 集成测试（设置 TASKCENTER_TEST_DSN 才运行，库需带完整计费表）。
// 人工处理：已确认的单子先退回已扣点数再标记失败；仍冻结 / 无预留单 / 已结束 / 已确认但无消费流水 都拒绝。
func TestAdminResolveStuckCopywriting(t *testing.T) {
	dsn := os.Getenv("TASKCENTER_TEST_DSN")
	if dsn == "" {
		t.Skip("TASKCENTER_TEST_DSN 未设置")
	}
	ctx := context.Background()
	conn := sqlx.NewMysql(dsn)
	const user = 88
	for _, q := range []string{
		"DELETE FROM afv_points_log_line WHERE user_id = 88",
		"DELETE FROM afv_points_log WHERE user_id = 88",
		"DELETE FROM afv_points_batch WHERE user_id = 88",
		"DELETE FROM afv_points_account WHERE user_id = 88",
		"DELETE FROM afv_billing_reservation WHERE user_id = 88",
		"DELETE FROM afv_copywriting WHERE user_id = 88",
	} {
		if _, err := conn.ExecCtx(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// 账户：已消费 1 点（余额 0）
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_account (user_id, balance, frozen_balance, total_consumed) VALUES (88, 0, 0, 1)"); err != nil {
		t.Fatal(err)
	}
	// 批次 9101：原 1 点，已全部消费
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_batch (id, account_id, user_id, amount, remaining, frozen, source) VALUES (9101, 0, 88, 1, 0, 0, 'TEST')"); err != nil {
		t.Fatal(err)
	}
	svcCtx := &svc.ServiceContext{
		Config:      config.Config{Generate: config.GenerateConf{TimeoutSeconds: 60}},
		Copywriting: model.NewAfvCopywritingModel(conn),
		Billing:     billing.New(billing.Conf{Mode: billing.ModeOn}, conn),
	}
	insert := func(status int64) int64 {
		res, err := conn.ExecCtx(ctx, "INSERT INTO afv_copywriting (user_id, source, title, body, word_count, topic, status) "+
			"VALUES (88, 'AI_WRITE', '', '', 0, '主题', ?)", status)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	reserve := func(id int64, status string) string {
		no := "BR-ADM-" + strconv.FormatInt(id, 10)
		if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_billing_reservation (user_id, biz_type, biz_id, status, price_points, feature_code, reservation_no, idempotency_key) "+
			"VALUES (88, 'LOCALLIFE', ?, ?, 1, 'locallife_text_gen', ?, ?)",
			"88_copywriting-"+strconv.FormatInt(id, 10), status, no, "IK-"+no); err != nil {
			t.Fatal(err)
		}
		return no
	}
	// 已确认的消费流水：CONSUME 流水 + 明细，幂等键 confirm:{no}
	consume := func(no string) {
		if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_log (ledger_no, user_id, change_type, amount, balance_after, idempotency_key) "+
			"VALUES (?, 88, 'CONSUME', -1, 0, ?)", "LG-"+no, "confirm:"+no); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_log_line (ledger_id, batch_id, user_id, amount, before_remaining, after_remaining, before_frozen, after_frozen) "+
			"SELECT id, 9101, 88, -1, 1, 1, 0, 0 FROM afv_points_log WHERE idempotency_key = ?", "confirm:"+no); err != nil {
			t.Fatal(err)
		}
	}

	confirmed := insert(model.StatusGenerating)
	confirmedNo := reserve(confirmed, "CONFIRMED")
	consume(confirmedNo)

	held := insert(model.StatusGenerating)
	reserve(held, "HELD")
	unbilled := insert(model.StatusGenerating)
	done := insert(model.StatusDone)
	reserve(done, "CONFIRMED")
	orphan := insert(model.StatusGenerating) // 已确认但没有消费流水：拒绝
	reserve(orphan, "CONFIRMED")

	resolve := func(id int64, note string) (*pb.AdminResolveResp, error) {
		return NewAdminResolveStuckCopywritingLogic(ctx, svcCtx).AdminResolveStuckCopywriting(&pb.AdminResolveReq{Id: id, Note: note})
	}
	if _, err := resolve(confirmed, "  "); err == nil {
		t.Error("空处理说明应拒绝")
	}

	resp, err := resolve(confirmed, "用户反馈未出结果，已核对")
	if err != nil {
		t.Fatalf("已确认的卡住记录应可人工处理: %v", err)
	}
	if resp.ReservationStatus != "REFUNDED" || resp.RefundedPoints != 1 {
		t.Errorf("应退回 1 点并返回 REFUNDED，实际 status=%q refunded=%d", resp.ReservationStatus, resp.RefundedPoints)
	}

	var st int64
	var reason string
	if err := conn.QueryRowCtx(ctx, &st, "SELECT status FROM afv_copywriting WHERE id = ?", confirmed); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowCtx(ctx, &reason, "SELECT fail_reason FROM afv_copywriting WHERE id = ?", confirmed); err != nil {
		t.Fatal(err)
	}
	if st != model.StatusFailed || !strings.Contains(reason, "用户反馈未出结果") {
		t.Errorf("应标记失败并写入处理说明，实际 status=%d reason=%q", st, reason)
	}
	var balance, consumedTotal, refundedTotal, batchRemaining int64
	_ = conn.QueryRowCtx(ctx, &balance, "SELECT balance FROM afv_points_account WHERE user_id = 88")
	_ = conn.QueryRowCtx(ctx, &consumedTotal, "SELECT total_consumed FROM afv_points_account WHERE user_id = 88")
	_ = conn.QueryRowCtx(ctx, &refundedTotal, "SELECT total_refunded FROM afv_points_account WHERE user_id = 88")
	_ = conn.QueryRowCtx(ctx, &batchRemaining, "SELECT remaining FROM afv_points_batch WHERE id = 9101")
	if balance != 1 || consumedTotal != 0 || refundedTotal != 1 || batchRemaining != 1 {
		t.Errorf("点数应退回原批次：balance=%d（期望 1）total_consumed=%d（期望 0）total_refunded=%d（期望 1）batch.remaining=%d（期望 1）",
			balance, consumedTotal, refundedTotal, batchRemaining)
	}

	// 再次处理已结束的记录：拒绝，不会重复退款
	if _, err := resolve(confirmed, "重复点击"); err == nil {
		t.Error("已结束的记录不应再次处理")
	}
	for name, id := range map[string]int64{"仍冻结": held, "无预留单": unbilled, "已结束": done, "已确认但无消费流水": orphan} {
		if _, err := resolve(id, "处理"); err == nil {
			t.Errorf("%s 的记录不应允许人工处理", name)
		}
	}
}
