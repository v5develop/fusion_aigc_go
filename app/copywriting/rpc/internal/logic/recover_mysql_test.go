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
// 真实 MySQL 集成测试（设置 TASKCENTER_TEST_DSN 才运行），表结构同 taskcenter 的集成测试。
// 三种对账结果：没有预留单 → 失败；预留单已确认 → 不动（人工核对）；预留单已退回 → 失败。
func TestStuckRecoveryReconcilesWithBilling(t *testing.T) {
	dsn := os.Getenv("TASKCENTER_TEST_DSN")
	if dsn == "" {
		t.Skip("TASKCENTER_TEST_DSN 未设置")
	}
	ctx := context.Background()
	conn := sqlx.NewMysql(dsn)
	for _, table := range []string{"afv_billing_reservation", "afv_copywriting"} {
		if _, err := conn.ExecCtx(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	svcCtx := &svc.ServiceContext{
		Config:      config.Config{Generate: config.GenerateConf{TimeoutSeconds: 60}},
		Copywriting: model.NewAfvCopywritingModel(conn),
		Billing:     billing.New(billing.Conf{Mode: billing.ModeOn}, conn),
	}

	insert := func(label string) int64 {
		res, err := conn.ExecCtx(ctx, "INSERT INTO afv_copywriting (user_id, source, title, body, word_count, topic, status, update_time) "+
			"VALUES (7, 'AI_WRITE', '', '', 0, ?, 0, NOW() - INTERVAL 1 HOUR)", label)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	reserve := func(id int64, status string) {
		if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_billing_reservation (user_id, biz_type, biz_id, status, price_points, feature_code, reservation_no, idempotency_key) "+
			"VALUES (7, 'LOCALLIFE', ?, ?, 1, 'locallife_text_gen', ?, ?)",
			"7_copywriting-"+strconv.FormatInt(id, 10), status, "BR-"+strconv.FormatInt(id, 10), "IK-"+strconv.FormatInt(id, 10)); err != nil {
			t.Fatal(err)
		}
	}
	noBilling := insert("无预留")
	confirmed := insert("已确认")
	reserve(confirmed, "CONFIRMED")
	refunded := insert("已退回")
	reserve(refunded, "REFUNDED")

	NewStuckRecovery(svcCtx).sweep(ctx)

	status := func(id int64) int64 {
		var s int64
		if err := conn.QueryRowCtx(ctx, &s, "SELECT status FROM afv_copywriting WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := status(noBilling); got != model.StatusFailed {
		t.Errorf("无预留单应标记失败，实际 status=%d", got)
	}
	if got := status(refunded); got != model.StatusFailed {
		t.Errorf("预留单已退回应标记失败，实际 status=%d", got)
	}
	if got := status(confirmed); got != model.StatusGenerating {
		t.Errorf("预留单已确认不应自动处理，实际 status=%d", got)
	}
}
