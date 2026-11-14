package asset

// 真库集成（ASSET_TEST_DSN，库名须含 scratch；表由本测试建）：幂等、崩溃在两步之间、并发补入只入一次。
//
//	mysql -e 'CREATE DATABASE asset_scratch_x' && ASSET_TEST_DSN='root:123456@tcp(127.0.0.1:43306)/asset_scratch_x?parseTime=true' go test ./pkg/asset/ -run Integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var integrationDDL = []string{
	"DROP TABLE IF EXISTS afv_asset, afv_asset_item, afv_workspace, afv_workspace_member, afv_workspace_tag, afv_app_user, t_task",
	"CREATE TABLE afv_asset (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, user_id bigint, workspace_id bigint NOT NULL, " +
		"source_task_id varchar(128), type varchar(64), entry_code varchar(64) NOT NULL DEFAULT '', name varchar(128) NOT NULL, " +
		"cover_url varchar(1024), result_url varchar(1024), watermark_url varchar(1024), aspect_ratio varchar(16) NOT NULL DEFAULT '', " +
		"properties json, ai_prompt text, sys_tags json, source_type int DEFAULT 1, status int DEFAULT 1, deleted tinyint NOT NULL DEFAULT 0, " +
		"KEY idx_asset_source_task (source_task_id)) ENGINE=InnoDB",
	"CREATE TABLE afv_asset_item (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, asset_id bigint, item_type varchar(32), name varchar(128), " +
		"media_url varchar(1024), media_type varchar(16), sort_order bigint, ai_prompt text, source_type int, deleted tinyint NOT NULL DEFAULT 0) ENGINE=InnoDB",
	"CREATE TABLE afv_workspace (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, name varchar(255) NOT NULL, owner_user_id bigint NOT NULL, " +
		"kind varchar(16) NOT NULL, status int NOT NULL DEFAULT 1, deleted tinyint NOT NULL DEFAULT 0, KEY idx_owner (owner_user_id, kind, deleted)) ENGINE=InnoDB",
	"CREATE TABLE afv_workspace_member (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, workspace_id bigint NOT NULL, user_id bigint NOT NULL, " +
		"role int NOT NULL DEFAULT 3, status int NOT NULL DEFAULT 1, deleted tinyint NOT NULL DEFAULT 0, UNIQUE KEY uk (workspace_id, user_id)) ENGINE=InnoDB",
	"CREATE TABLE afv_workspace_tag (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, workspace_id bigint NOT NULL, title varchar(64) NOT NULL, " +
		"creator_user_id bigint NOT NULL DEFAULT 0, deleted tinyint NOT NULL DEFAULT 0) ENGINE=InnoDB",
	"CREATE TABLE afv_app_user (id bigint NOT NULL PRIMARY KEY, nickname varchar(64), deleted tinyint NOT NULL DEFAULT 0) ENGINE=InnoDB",
	"CREATE TABLE t_task (id bigint NOT NULL PRIMARY KEY, status int NOT NULL DEFAULT 0) ENGINE=InnoDB",
	"INSERT INTO afv_app_user (id, nickname) VALUES (7, 'u7')",
	// 个人空间预先建好：并发写入时不在 EnsurePersonal 上互相竞争（与线上一致：用户早有个人空间）
	"INSERT INTO afv_workspace (id, name, owner_user_id, kind) VALUES (3, 'u7的个人空间', 7, 'PERSONAL')",
	"INSERT INTO afv_workspace_member (workspace_id, user_id, role) VALUES (3, 7, 1)",
}

func integrationConn(t *testing.T) sqlx.SqlConn {
	t.Helper()
	dsn := os.Getenv("ASSET_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ASSET_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") {
		t.Fatal("ASSET_TEST_DSN 库名须含 scratch（会建表 / 删表）")
	}
	conn := sqlx.NewMysql(dsn)
	for _, q := range integrationDDL {
		if _, err := conn.ExecCtx(context.Background(), q); err != nil {
			t.Fatalf("建表失败: %v\n%s", err, q)
		}
	}
	return conn
}

func countAssets(t *testing.T, conn sqlx.SqlConn, src string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowCtx(context.Background(), &n, "SELECT COUNT(1) FROM afv_asset WHERE source_task_id = ?", src); err != nil {
		t.Fatal(err)
	}
	return n
}

// retryUntil 并发写入时 InnoDB 可能判死锁回滚其一（返回错误）：调用方的语义是「下一轮重试」，这里就地重试到成功。
func retryUntil(t *testing.T, fn func() error) {
	t.Helper()
	var err error
	for i := 0; i < 20; i++ {
		if err = fn(); err == nil {
			return
		}
	}
	t.Fatalf("重试 20 次仍失败: %v", err)
}

// 幂等：同一组写两次只有一组；补缺：删掉序号 1 的行后（模拟只写了部分——实际不会，因为整组同事务）再写只补那一行；
// 用户删除的行（deleted=1）不会被补回。
func TestIntegrationInsertSetOnceIdx(t *testing.T) {
	conn := integrationConn(t)
	w := NewWriter(conn)
	ctx := context.Background()
	if n, err := w.InsertSetOnceIdx(ctx, set3("g1")); err != nil || n != 3 {
		t.Fatalf("首写应写 3 行: %d %v", n, err)
	}
	if n, err := w.InsertSetOnceIdx(ctx, set3("g1")); err != nil || n != 0 {
		t.Fatalf("重写不应再写: %d %v", n, err)
	}
	if _, err := conn.ExecCtx(ctx, "DELETE FROM afv_asset WHERE source_task_id = 'g1' AND JSON_EXTRACT(properties, '$.outputSeq') = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecCtx(ctx, "UPDATE afv_asset SET deleted = 1 WHERE source_task_id = 'g1'"); err != nil {
		t.Fatal(err)
	}
	if n, err := w.InsertSetOnceIdx(ctx, set3("g1")); err != nil || n != 1 || countAssets(t, conn, "g1") != 3 {
		t.Fatalf("只补缺失的序号 1（已删的 0、2 不补回）: %d %v", n, err)
	}
	// 旧写法的整组（不带序号）挡住整组
	if err := w.InsertSet(ctx, set3("g2")[:1]); err != nil {
		t.Fatal(err)
	}
	if n, err := w.InsertSetOnceIdx(ctx, set3("g2")); err != nil || n != 0 || countAssets(t, conn, "g2") != 1 {
		t.Fatalf("旧写法整组应视为已入库: %d %v", n, err)
	}
}

// 并发补入只入一次：8 个协程同时写同一组（InsertSetOnceIdx 与画布的整组查重 InsertSetOnce 各测一遍），最终恰好一组。
func TestIntegrationConcurrentInsertOnce(t *testing.T) {
	conn := integrationConn(t)
	w := NewWriter(conn)
	ctx := context.Background()
	run := func(src string, fn func() error) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				retryUntil(t, fn)
			}()
		}
		wg.Wait()
		if n := countAssets(t, conn, src); n != 3 {
			t.Fatalf("%s 并发写入后应恰好 3 行: %d", src, n)
		}
	}
	run("c1", func() error { _, err := w.InsertSetOnceIdx(ctx, set3("c1")); return err })
	run("c2", func() error { _, err := w.InsertSetOnce(ctx, set3("c2")); return err })
}

func finishTask(id int64) func(ctx context.Context, s sqlx.Session) (bool, error) {
	return func(ctx context.Context, s sqlx.Session) (bool, error) {
		r, err := s.ExecCtx(ctx, "UPDATE t_task SET status = 1 WHERE id = ? AND status = 0", id)
		if err != nil {
			return false, err
		}
		n, err := r.RowsAffected()
		return n > 0, err
	}
}

// 置完成与入库同事务：入库失败 → 入库这一步连同置完成一起回滚，再不带入库置完成（交付不被拦）；
// 「崩溃在两步之间」= 任务已完成而资产行没有 → 对账补入恰好一组，再扫不重复。
func TestIntegrationFinishWithAssetsAndReconcile(t *testing.T) {
	conn := integrationConn(t)
	w := NewWriter(conn)
	ctx := context.Background()
	if _, err := conn.ExecCtx(ctx, "INSERT INTO t_task (id, status) VALUES (1, 0), (2, 0)"); err != nil {
		t.Fatal(err)
	}
	// 1：写入方写了两行后报错 → 全部回滚，退回只置完成
	res, err := w.FinishWithAssets(ctx, finishTask(1), func(ctx context.Context, s sqlx.Session) error {
		if _, err := InsertSetOnceTx(ctx, s, set3("f1")[:2]); err != nil {
			return err
		}
		return errors.New("模拟入库失败")
	})
	if err != nil || !res.OK || res.AssetErr == nil {
		t.Fatalf("应退回只置完成: %+v %v", res, err)
	}
	var st int
	_ = conn.QueryRowCtx(ctx, &st, "SELECT status FROM t_task WHERE id = 1")
	if st != 1 || countAssets(t, conn, "f1") != 0 {
		t.Fatalf("任务应已完成、入库那半步应已回滚: status=%d assets=%d", st, countAssets(t, conn, "f1"))
	}
	// 对账补入（崩溃在两步之间的形态）：补一组，再补不重复
	for i, want := range []int{3, 0} {
		if n, err := w.InsertSetOnceIdx(ctx, set3("f1")); err != nil || n != want {
			t.Fatalf("第 %d 次对账补入: n=%d err=%v", i+1, n, err)
		}
	}
	// 2：收尾（同事务入库）与对账并发：恰好一组
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		retryUntil(t, func() error {
			_, err := w.FinishWithAssets(ctx, finishTask(2), func(ctx context.Context, s sqlx.Session) error {
				_, err := InsertSetOnceTx(ctx, s, set3("f2"))
				return err
			})
			return err
		})
	}()
	go func() {
		defer wg.Done()
		retryUntil(t, func() error { _, err := w.InsertSetOnceIdx(ctx, set3("f2")); return err })
	}()
	wg.Wait()
	if n := countAssets(t, conn, "f2"); n != 3 {
		t.Fatalf("收尾与对账并发应恰好一组: %d", n)
	}
	// 主资产 + 子资产的幂等写入
	a := Asset{UserID: 7, SourceTaskID: "p1", Type: "customizedModel", Name: "形象", ResultURL: "https://cdn/p"}
	items := []Item{{ItemType: "variant", MediaURL: "https://cdn/p0", MediaType: "image"}, {ItemType: "variant", MediaURL: "https://cdn/p1", MediaType: "image", SortOrder: 1}}
	for i, want := range []bool{true, false} {
		if wrote, err := w.InsertWithItemsOnce(ctx, a, items); err != nil || wrote != want {
			t.Fatalf("第 %d 次写形象: wrote=%v err=%v", i+1, wrote, err)
		}
	}
	var ni int
	_ = conn.QueryRowCtx(ctx, &ni, "SELECT COUNT(1) FROM afv_asset_item")
	if countAssets(t, conn, "p1") != 1 || ni != 2 {
		t.Fatalf("形象应只有一行主资产 + 2 条子资产: assets=%d items=%d", countAssets(t, conn, "p1"), ni)
	}
	// 编辑后整组重写：旧行逻辑删除、新行带序号，同事务
	if err := w.ReplaceSet(ctx, set3("f2")[:2]); err != nil {
		t.Fatal(err)
	}
	var live int
	_ = conn.QueryRowCtx(ctx, &live, "SELECT COUNT(1) FROM afv_asset WHERE source_task_id = 'f2' AND deleted = 0")
	if live != 2 || countAssets(t, conn, "f2") != 5 {
		t.Fatalf("重写后应 2 行在用、3 行已删: live=%d all=%d", live, countAssets(t, conn, "f2"))
	}
}

// 对账器端到端：任务表里已完成而素材库缺行的被补入，跑两遍只补一次（已有行 / 未完成的不动）。
func TestIntegrationReconcilerEndToEnd(t *testing.T) {
	conn := integrationConn(t)
	w := NewWriter(conn)
	ctx := context.Background()
	if _, err := conn.ExecCtx(ctx, "INSERT INTO t_task (id, status) VALUES (11, 1), (12, 1), (13, 0), (14, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.InsertSetOnceIdx(ctx, set3("r12")); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Service: "it", Conf: ReconcileConf{Enabled: true, WindowHours: 1, Batch: 1}, Writer: w,
		List: func(ctx context.Context, _, _ time.Time, after ReconcileCursor, limit int) ([]ReconcileItem, error) {
			var ids []int64
			if err := conn.QueryRowsCtx(ctx, &ids, "SELECT id FROM t_task WHERE status = 1 AND id > ? ORDER BY id LIMIT ?", after.ID, limit); err != nil {
				return nil, err
			}
			var out []ReconcileItem
			for _, id := range ids {
				out = append(out, ReconcileItem{ID: id, UserID: 7, SourceTaskID: fmt.Sprintf("r%d", id)})
			}
			return out, nil
		},
		Fix: func(ctx context.Context, it ReconcileItem) (bool, error) {
			n, err := w.InsertSetOnceIdx(ctx, set3(it.SourceTaskID))
			return n > 0, err
		},
	}
	st := r.RunOnce(ctx)
	if st.Scanned != 3 || st.Missing != 2 || st.Inserted != 2 || st.Failed != 0 {
		t.Fatalf("第一遍: %+v", st)
	}
	if st := r.RunOnce(ctx); st.Missing != 0 || st.Inserted != 0 {
		t.Fatalf("第二遍不应再补: %+v", st)
	}
	for src, want := range map[string]int{"r11": 3, "r12": 3, "r13": 0, "r14": 3} {
		if n := countAssets(t, conn, src); n != want {
			t.Fatalf("%s 应有 %d 行: %d", src, want, n)
		}
	}
}
