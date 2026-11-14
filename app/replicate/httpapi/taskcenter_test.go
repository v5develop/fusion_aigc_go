package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var tcDDL = []string{
	`CREATE TABLE IF NOT EXISTS afv_creation_task (
  id BIGINT NOT NULL AUTO_INCREMENT, task_id VARCHAR(64) NOT NULL, user_id BIGINT NOT NULL, type VARCHAR(64) NOT NULL,
  status INT NOT NULL DEFAULT 0, entry_code VARCHAR(64) NOT NULL DEFAULT '', request_params JSON NULL,
  execution_task_id VARCHAR(128) NULL, execution_params JSON NULL, billing_status VARCHAR(32) NULL, price_points BIGINT NULL,
  feature_code VARCHAR(64) NULL, model_id BIGINT NULL, reservation_no VARCHAR(64) NULL, result_url VARCHAR(2048) NULL,
  result_urls JSON NULL, cover_url VARCHAR(2048) NULL, error_msg TEXT NULL, is_transferred TINYINT NOT NULL DEFAULT 0,
  deleted TINYINT NOT NULL DEFAULT 0, origin_service VARCHAR(32) NULL, origin_ref VARCHAR(64) NULL,
  create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id), UNIQUE KEY uk_task_id (task_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS afv_creation_task_text (id BIGINT NOT NULL AUTO_INCREMENT, task_id VARCHAR(64) NOT NULL, content MEDIUMTEXT NOT NULL,
  create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id), UNIQUE KEY uk_task_id (task_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS afv_billing_reservation (id BIGINT NOT NULL AUTO_INCREMENT, reservation_no VARCHAR(64) NOT NULL, user_id BIGINT NOT NULL,
  feature_code VARCHAR(64) NOT NULL, model_id BIGINT NULL, price_points BIGINT NOT NULL, biz_type VARCHAR(32) NOT NULL, biz_id VARCHAR(64) NOT NULL,
  attempt_no INT NOT NULL DEFAULT 0, status VARCHAR(32) NOT NULL, idempotency_key VARCHAR(128) NOT NULL, deleted TINYINT NOT NULL DEFAULT 0,
  create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id), UNIQUE KEY uk_reservation_no (reservation_no)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS afv_exec_submission (exec_task_id VARCHAR(128) NOT NULL, task_type INT NOT NULL, routed_task_type INT NULL, input JSON NULL,
  error_msg VARCHAR(512) NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (exec_task_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// 集成（REPLICATE_TEST_DSN，已执行 Go_V1.0.37 + Go_V1.0.39 的临时库）：视频做同款作为任务中心新同步源，legacy / new 两种模式结果一致。
func TestIntegrationTaskSource(t *testing.T) {
	dsn := os.Getenv("REPLICATE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 REPLICATE_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") {
		t.Fatalf("必须是临时库：%s", dsn)
	}
	ctx := context.Background()
	conn := sqlx.NewMysql(dsn)
	for _, q := range tcDDL {
		if _, err := conn.ExecCtx(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	const uid = 990088
	clean := func() {
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_creation_task WHERE user_id = ?", uid)
		_, _ = conn.ExecCtx(ctx, "DELETE c FROM afv_replicate_charge c JOIN afv_replicate_job j ON j.id = c.job_id WHERE j.user_id = ?", uid)
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_replicate_delivery WHERE user_id = ?", uid)
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_replicate_job WHERE user_id = ?", uid)
	}
	clean()
	t.Cleanup(clean)
	res, err := conn.ExecCtx(ctx, "INSERT INTO afv_replicate_job (user_id, request_id, plan_id, plan_no, plan_hash, status, input, seg_total, seg_done, seg_failed) "+
		"VALUES (?, 'tc1', 1, 'VRPtc', 'h', 7, '{\"title\":\"3 个镜头：模特展示外套\"}', 3, 1, 0)", uid)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	ref := taskcenter.Ref("rpl", id)
	_, _ = conn.ExecCtx(ctx, "UPDATE afv_replicate_job SET task_id = ? WHERE id = ?", ref, id)
	for i, st := range []int{2, 1, 3, 6} { // 已扣除 120、预留中 80、已退回 50（不算）、扣费确认中 30（算）
		reason := "x"
		if st == 6 {
			reason = "confirm: db down"
		}
		pts := []int{120, 80, 50, 30}[i]
		if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_replicate_charge (job_id, user_id, seg_index, stage, request_id, seconds, points, status, reason) VALUES (?, ?, ?, 'first', ?, 5, ?, ?, ?)",
			id, uid, i+1, ref+string(rune('a'+i)), pts, st, reason); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		Status    int            `db:"status"`
		Type      string         `db:"type"`
		Entry     string         `db:"entry_code"`
		Params    string         `db:"request_params"`
		Result    sql.NullString `db:"result_url"`
		Cover     sql.NullString `db:"cover_url"`
		Billing   sql.NullString `db:"billing_status"`
		Origin    string         `db:"origin_service"`
		OriginRef string         `db:"origin_ref"`
	}
	read := func() row {
		var r row
		if err := conn.QueryRowCtx(ctx, &r, "SELECT status, type, entry_code, request_params, result_url, cover_url, billing_status, origin_service, origin_ref FROM afv_creation_task WHERE task_id = ?", ref); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, mode := range []taskcenter.SyncMode{taskcenter.ModeLegacy, taskcenter.ModeNew} {
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_creation_task WHERE user_id = ?", uid)
		_, _ = conn.ExecCtx(ctx, "UPDATE afv_replicate_job SET status = 7, seg_done = 1, seg_failed = 0, tc_updated_at = NOW(3) WHERE id = ?", id)
		_, _ = conn.ExecCtx(ctx, "DELETE FROM afv_replicate_delivery WHERE job_id = ?", id)
		y := taskcenter.New(conn, 10*time.Minute, TaskSource()).WithMode(mode)
		if err := y.Sync(ctx); err != nil {
			t.Fatalf("%s：%v", mode, err)
		}
		r := read()
		var p map[string]any
		_ = json.Unmarshal([]byte(r.Params), &p)
		if r.Status != 1 || r.Type != "video" || r.Entry != "video_replicate" || r.Origin != "LOCAL_LIFE" || r.OriginRef != "rpl:"+itoa(id) ||
			r.Result.Valid || r.Billing.String != "NONE" {
			t.Fatalf("%s 待继续应同步为处理中、未入库不带地址：%+v", mode, r)
		}
		if p["progress"].(float64) != 33 || p["points"].(float64) != 230 || p["link"] != "/video/replicate?job="+itoa(id) ||
			p["title"] != "3 个镜头：模特展示外套" || p["segTotal"].(float64) != 3 || p["entryCode"] != "video_replicate" {
			t.Fatalf("%s 进度 / 点数 / 跳转：%v", mode, p)
		}
		// 完成 + 入库：result_url 取 完整成片 > 预览版 > 第 1 个已入库的段（封面取同一行，没有回退该行地址）
		_, _ = conn.ExecCtx(ctx, "UPDATE afv_replicate_job SET status = 5, seg_done = 2, seg_failed = 1 WHERE id = ?", id)
		for _, x := range []struct {
			ins, wantURL, wantCover string
		}{
			{"(?, ?, 'segment', 3, 'r3', 1, 'https://pub.test/s3.mp4', NULL)", "https://pub.test/s3.mp4", "https://pub.test/s3.mp4"},
			{"(?, ?, 'segment', 1, 'r1', 1, 'https://pub.test/s1.mp4', 'https://pub.test/s1.jpg')", "https://pub.test/s1.mp4", "https://pub.test/s1.jpg"},
			{"(?, ?, 'preview', 0, 'rp', 1, 'https://pub.test/preview.mp4', NULL)", "https://pub.test/preview.mp4", "https://pub.test/preview.mp4"},
			{"(?, ?, 'final', 0, 'r', 1, 'https://pub.test/final.mp4', NULL)", "https://pub.test/final.mp4", "https://pub.test/final.mp4"},
		} {
			time.Sleep(1100 * time.Millisecond) // update_time 秒级精度
			if _, err := conn.ExecCtx(ctx, "INSERT INTO afv_replicate_delivery (job_id, user_id, kind, seg_index, src_rid, status, video_url, cover_url) VALUES "+x.ins, id, uid); err != nil {
				t.Fatal(err)
			}
			_, _ = conn.ExecCtx(ctx, "UPDATE afv_replicate_job SET tc_updated_at = NOW(3), updated_at = updated_at WHERE id = ?", id)
			if err := y.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			r = read()
			if r.Status != 5 || r.Result.String != x.wantURL || r.Cover.String != x.wantCover {
				t.Fatalf("%s 入库 %s 后 result/cover 应为 %s / %s：%+v", mode, x.ins, x.wantURL, x.wantCover, r)
			}
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
