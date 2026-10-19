package logic

import (
	"context"
	"os"
	"strconv"
	"testing"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/config"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 与其他集成测试共用同一个库，请用 go test -p 1 串行运行。
// 真实 MySQL 集成测试（设置 TASKCENTER_TEST_DSN 才运行，库需带完整计费表）：
// 卡住的 HELD 记录被回收时，文案标记失败，冻结的点数必须真的退回账户。
// 冻结流水（points_log FREEZE + points_log_line + points_batch）按真实冻结的形状手工造。
func TestStuckRecoveryRefundsHeldPoints(t *testing.T) {
	dsn := os.Getenv("TASKCENTER_TEST_DSN")
	if dsn == "" {
		t.Skip("TASKCENTER_TEST_DSN 未设置")
	}
	ctx := context.Background()
	conn := sqlx.NewMysql(dsn)
	const user = 77
	for _, q := range []string{
		"DELETE FROM afv_points_log_line WHERE user_id = 77",
		"DELETE FROM afv_points_log WHERE user_id = 77",
		"DELETE FROM afv_points_batch WHERE user_id = 77",
		"DELETE FROM afv_points_account WHERE user_id = 77",
		"DELETE FROM afv_billing_reservation WHERE user_id = 77",
		"DELETE FROM afv_copywriting WHERE user_id = 77",
	} {
		if _, err := conn.ExecCtx(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_account (user_id, balance, frozen_balance) VALUES (77, 0, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_batch (id, account_id, user_id, amount, remaining, frozen, source) VALUES (9001, 0, 77, 1, 0, 1, 'TEST')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_log (id, ledger_no, user_id, change_type, amount, balance_after, idempotency_key) VALUES (9001, 'LG-TEST', 77, 'FREEZE', -1, 0, 'IK-TEST-1')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_points_log_line (ledger_id, batch_id, user_id, amount, before_remaining, after_remaining) VALUES (9001, 9001, 77, -1, 1, 0)"); err != nil {
		t.Fatal(err)
	}

	res, err := conn.ExecCtx(ctx, "INSERT INTO afv_copywriting (user_id, source, title, body, word_count, topic, status, update_time) "+
		"VALUES (77, 'AI_WRITE', '', '', 0, '主题', 0, NOW() - INTERVAL 1 HOUR)")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_billing_reservation (user_id, biz_type, biz_id, status, price_points, feature_code, reservation_no, idempotency_key) "+
		"VALUES (77, 'LOCALLIFE', ?, 'HELD', 1, 'locallife_text_gen', 'BR-TEST-REFUND', 'IK-TEST-1')",
		"77_copywriting-"+strconv.FormatInt(id, 10)); err != nil {
		t.Fatal(err)
	}

	svcCtx := &svc.ServiceContext{
		Config:      config.Config{Generate: config.GenerateConf{TimeoutSeconds: 60}},
		Copywriting: model.NewAfvCopywritingModel(conn),
		Billing:     billing.New(billing.Conf{Mode: billing.ModeOn}, conn),
	}
	NewStuckRecovery(svcCtx).sweep(ctx)

	var st int64
	if err := conn.QueryRowCtx(ctx, &st, "SELECT status FROM afv_copywriting WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if st != model.StatusFailed {
		t.Errorf("文案应标记失败，实际 status=%d", st)
	}
	var resv string
	if err := conn.QueryRowCtx(ctx, &resv, "SELECT status FROM afv_billing_reservation WHERE user_id = 77"); err != nil {
		t.Fatal(err)
	}
	if resv != "REFUNDED" {
		t.Errorf("预留单应退回，实际 %s", resv)
	}
	var frozen, balance int64
	if err := conn.QueryRowCtx(ctx, &frozen, "SELECT frozen_balance FROM afv_points_account WHERE user_id = 77"); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowCtx(ctx, &balance, "SELECT balance FROM afv_points_account WHERE user_id = 77"); err != nil {
		t.Fatal(err)
	}
	if frozen != 0 || balance != 1 {
		t.Errorf("点数应退回账户：frozen=%d（期望 0）balance=%d（期望 1）", frozen, balance)
	}
}
