package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/bundb"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 对真实 MySQL：PLAZA_TEST_DSN 指向已执行 Go_V1.0.33 / 35 的临时库（库名含 scratch / test），否则跳过。
// 测试会清空 afv_compat_plaza_template 与 afv_agent_turn。
func TestIntegrationCatalogVisibilityAndTurnsBound(t *testing.T) {
	dsn := os.Getenv("PLAZA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PLAZA_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Fatalf("PLAZA_TEST_DSN 必须指向临时库：%s", dsn)
	}
	db, err := bundb.New(sqlx.NewMysql(dsn))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []string{"DELETE FROM afv_compat_plaza_template", "DELETE FROM afv_agent_turn",
		// 1: PUBLISHED + enabled=1；2: PUBLISHED + enabled=0（Java 后台下线但 state 未同步）
		`INSERT INTO afv_compat_plaza_template (id, title, data_json, enabled, deleted, state) VALUES
		 (1, 'a', '{"result":["https://x/a.webp"]}', 1, 0, 'PUBLISHED'),
		 (2, 'b', '{"result":["https://x/b.webp"]}', 0, 0, 'PUBLISHED')`,
		`INSERT INTO afv_agent_turn (id, user_id, conversation_id, turn_no, turn_id, run_id, status, plaza_template_id, create_time, update_time) VALUES
		 (10, 1, 'c1', 1, 't10', 'r10', 'completed', 1, NOW() - INTERVAL 1 HOUR, NOW()),
		 (11, 2, 'c2', 1, 't11', 'r11', 'completed', 1, NOW() - INTERVAL 1 HOUR, NOW()),
		 (12, 3, 'c3', 1, 't12', 'r12', 'completed', 1, NOW() - INTERVAL 1 HOUR, NOW())`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := time.Now()
	for _, tc := range []struct {
		requireEnabled bool
		want2          bool
	}{{false, true}, {true, false}} {
		r := &Repo{DB: db, RequireEnabled: tc.requireEnabled}
		cat, err := r.LoadCatalog(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if !cat.Templates[1].VisibleAt(now) {
			t.Fatalf("requireEnabled=%v：模板 1 应可见", tc.requireEnabled)
		}
		if got := cat.Templates[2].VisibleAt(now); got != tc.want2 {
			t.Fatalf("requireEnabled=%v：模板 2 可见=%v want %v", tc.requireEnabled, got, tc.want2)
		}
	}
	r := New(db)
	all, err := r.TurnsBetween(ctx, now.Add(-2*time.Hour), now, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("不限水位应 3 行: %d %v", len(all), err)
	}
	bounded, err := r.TurnsBetween(ctx, now.Add(-2*time.Hour), now, 11)
	if err != nil || len(bounded) != 2 {
		t.Fatalf("水位 11 应 2 行: %d %v", len(bounded), err)
	}
}
