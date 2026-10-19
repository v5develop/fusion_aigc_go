package svc

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 与其他集成测试共用同一个库，请用 go test -p 1 串行运行。
// 真实 MySQL 集成测试。设置 TASKCENTER_TEST_DSN（不带 collation 参数）才会运行。
// 表结构需先建好：afv_copywriting / afv_creation_task / afv_creation_task_text / afv_billing_reservation / afv_exec_submission。
// 库的默认排序规则要与线上一致（MySQL 8 默认 utf8mb4_0900_ai_ci），这样才能覆盖排序规则问题。
//
// 先用不带 collation 的连接同步，服务端默认排序规则是 utf8mb4_0900_ai_ci 时应当报 1267（复现线上问题）；
// 再用带 collation=utf8mb4_unicode_ci 的连接同步，必须成功并写入主表与正文扩展表。
func TestCopywritingTaskSyncOnMySQL(t *testing.T) {
	base := os.Getenv("TASKCENTER_TEST_DSN")
	if base == "" {
		t.Skip("TASKCENTER_TEST_DSN 未设置")
	}
	ctx := context.Background()
	bad := sqlx.NewMysql(base)
	good := sqlx.NewMysql(base + "&collation=utf8mb4_unicode_ci")

	for _, table := range []string{"afv_creation_task_text", "afv_creation_task", "afv_billing_reservation", "afv_copywriting"} {
		if _, err := good.ExecCtx(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	res, err := good.ExecCtx(ctx, "INSERT INTO afv_copywriting (user_id, source, title, body, word_count, topic, status) "+
		"VALUES (7, 'AI_WRITE', '咖啡馆', '温馨的街角咖啡馆', 9, '咖啡', 1)")
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	// 计费请求号与 generate_logic.go 的 billingRequestID 一致：copywriting-<id>
	if _, err := good.ExecCtx(ctx, "INSERT INTO afv_billing_reservation (biz_type, biz_id, status, price_points, feature_code, reservation_no) "+
		"VALUES ('LOCALLIFE', ?, 'CONFIRMED', 1, 'locallife_text_gen', 'BR-TEST')", "7_copywriting-"+strconv.FormatInt(id, 10)); err != nil {
		t.Fatal(err)
	}

	// 1) 不带 collation：复现问题
	badErr := taskcenter.New(bad, time.Minute, copywritingSource("aiCopywriting")).Sync(ctx)
	if badErr == nil || !strings.Contains(badErr.Error(), "1267") {
		t.Logf("不带 collation 的同步结果：%v（期望 1267；为 nil 说明服务端默认排序规则不是 utf8mb4_0900_ai_ci）", badErr)
	} else {
		t.Logf("已复现 1267：%v", badErr)
	}

	// 2) 带 collation：必须成功
	if err := taskcenter.New(good, time.Minute, copywritingSource("aiCopywriting")).Sync(ctx); err != nil {
		t.Fatalf("带 collation 的同步失败: %v", err)
	}

	taskID := taskcenter.Ref("cw", id)
	var task struct {
		Type          string `db:"type"`
		EntryCode     string `db:"entry_code"`
		BillingStatus string `db:"billing_status"`
		OriginService string `db:"origin_service"`
		OriginRef     string `db:"origin_ref"`
		Status        int64  `db:"status"`
		PricePoints   int64  `db:"price_points"`
	}
	if err := good.QueryRowCtx(ctx, &task, "SELECT type, entry_code, billing_status, origin_service, origin_ref, status, price_points "+
		"FROM afv_creation_task WHERE task_id = ?", taskID); err != nil {
		t.Fatalf("主表行查询失败: %v", err)
	}
	if task.Type != "copywriting" || task.EntryCode != "aiCopywriting" || task.BillingStatus != "CONFIRMED" ||
		task.OriginService != "LOCAL_LIFE" || task.OriginRef != "cw:"+strconv.FormatInt(id, 10) ||
		task.Status != 2 || task.PricePoints != 1 {
		t.Fatalf("主表字段不对: %+v", task)
	}

	var content string
	if err := good.QueryRowCtx(ctx, &content, "SELECT content FROM afv_creation_task_text WHERE task_id = ?", taskID); err != nil {
		t.Fatalf("正文行查询失败: %v", err)
	}
	if content != "温馨的街角咖啡馆" {
		t.Fatalf("正文不对: %q", content)
	}
}
