package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/assetlib/model"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

// 真库校验有效分类推算 / 多标签与系统标签筛选 / 系统标签字典 / 批量移除（MySQL 8，JSON_CONTAINS + JSON_TABLE）。
// 设置 ASSETLIB_TEST_MYSQL_DSN（如 root:pass@tcp(127.0.0.1:3306)/）才跑：建临时库、跑完 DROP。
func TestKindInferenceMySQL(t *testing.T) {
	dsn := os.Getenv("ASSETLIB_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 ASSETLIB_TEST_MYSQL_DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("assetlib_kind_tmp_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	// 用 defer 而不是 t.Cleanup：Cleanup 在所有 defer 之后才跑，那时连接已关，临时库删不掉
	defer func() {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
			t.Errorf("删除临时库 %s 失败: %v", dbName, err)
		}
		_ = admin.Close()
	}()
	sdb, err := sql.Open("mysql", dsn+dbName+"?multiStatements=true&parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	ddl := `
CREATE TABLE afv_asset (id bigint PRIMARY KEY AUTO_INCREMENT, user_id bigint, workspace_id bigint DEFAULT 1, source_task_id varchar(64),
  type varchar(64), entry_code varchar(64) DEFAULT '', name varchar(128) DEFAULT '', cover_url varchar(512), result_url varchar(512),
  watermark_url varchar(512), width int DEFAULT 0, height int DEFAULT 0, aspect_ratio varchar(16) DEFAULT '', source_type int,
  ai_prompt text, sys_tags json NULL, status int DEFAULT 1, deleted tinyint DEFAULT 0, create_time datetime DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE afv_asset_ext (asset_id bigint PRIMARY KEY, user_id bigint, kind enum('model','scene','makeup','hair','accessory',
  'product','storefront','dish','logo','poster','video_final','video_segment','reference','other') DEFAULT NULL, favorite tinyint DEFAULT 0,
  license varchar(16) DEFAULT '', metadata json, provenance json, current_version_id bigint,
  created_at datetime(3) DEFAULT CURRENT_TIMESTAMP(3), updated_at datetime(3) DEFAULT CURRENT_TIMESTAMP(3));
CREATE TABLE afv_asset_version (id bigint PRIMARY KEY AUTO_INCREMENT, asset_id bigint, image_url varchar(512));
CREATE TABLE afv_billing_offer (id bigint PRIMARY KEY AUTO_INCREMENT, entry_code varchar(64), category varchar(16), status varchar(16), deleted tinyint DEFAULT 0);
CREATE TABLE afv_asset_tag (id bigint PRIMARY KEY AUTO_INCREMENT, user_id bigint, name varchar(32), created_at datetime(3) DEFAULT CURRENT_TIMESTAMP(3), UNIQUE KEY uk (user_id, name));
CREATE TABLE afv_asset_tag_link (asset_id bigint, tag_id bigint, created_at datetime(3) DEFAULT CURRENT_TIMESTAMP(3), PRIMARY KEY (asset_id, tag_id));
INSERT INTO afv_billing_offer (entry_code, category, status) VALUES ('hitVideo', 'VIDEO', 'ENABLED'), ('textToImage', 'IMAGE', 'ENABLED');
INSERT INTO afv_asset (id, user_id, type, entry_code, sys_tags, source_type) VALUES
  (1, 7, 'image', 'textToImage', '["文生图","用途:门头门店","行业:餐饮"]', 2),
  (2, 7, 'productSetImages', 'productSetImages', NULL, 2),
  (3, 7, 'hitVideo', 'hitVideo', NULL, 2),
  (4, 7, 'image', 'studio_tryon', '["商拍换装","2K"]', 2),
  (5, 7, 'image', '', NULL, 1),
  (6, 7, 'customizedModel', 'aiExpertX', NULL, 2),
  (7, 7, 'video', 'video_replicate', '["视频做同款","用途:视频片段","第2段"]', 2),
  (8, 7, 'image', 'textToImage', '["文生图","用途:门头门店"]', 2),
  (9, 8, 'image', 'textToImage', '["文生图","用途:门头门店"]', 2);
INSERT INTO afv_asset_ext (asset_id, user_id, kind) VALUES (8, 7, 'logo');
INSERT INTO afv_asset_tag (id, user_id, name) VALUES (1, 7, '红'), (2, 7, '夏'), (3, 8, '红');
INSERT INTO afv_asset_tag_link (asset_id, tag_id) VALUES (1, 1), (1, 2), (2, 1), (9, 3);`
	if _, err := sdb.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	r := New(bun.NewDB(sdb, mysqldialect.New()), nil)

	// 1. 返回的有效分类与 kind_source，并与 Go 侧 InferKind 同口径
	rows, total, err := r.Asset.List(ctx, Filter{UserID: 7}, 50, 0)
	if err != nil || total != 8 {
		t.Fatalf("total=%d err=%v", total, err)
	}
	want := map[int64][2]string{
		1: {"storefront", "auto"}, 2: {"product", "auto"}, 3: {"video_final", "auto"}, 4: {"model", "auto"},
		5: {"", "auto"}, 6: {"model", "auto"}, 7: {"video_segment", "auto"}, 8: {"logo", "user"},
	}
	for _, row := range rows {
		w := want[row.ID]
		if row.EffKind.String != w[0] || row.KindSource() != w[1] {
			t.Errorf("asset %d: kind=%q source=%s want %v", row.ID, row.EffKind.String, row.KindSource(), w)
		}
		if row.KindSource() == model.KindSourceAuto {
			if g := model.InferKind(row.SysTagList(), row.EntryCode, row.Type.String, row.IsVideo); g != row.EffKind.String {
				t.Errorf("asset %d: Go 推算 %q 与 SQL %q 不一致", row.ID, g, row.EffKind.String)
			}
		}
	}

	ids := func(f Filter) string {
		f.UserID = 7
		rs, _, err := r.Asset.List(ctx, f, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range rs {
			out = append(out, fmt.Sprint(x.ID))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	// 2. 筛选组合
	checks := []struct {
		name string
		f    Filter
		want string
	}{
		{"kind=storefront（用户改过的 8 不算）", Filter{Kind: "storefront"}, "1"},
		{"kind=logo（用户分类）", Filter{Kind: "logo"}, "8"},
		{"未分类", Filter{Kind: "uncategorized"}, "5"},
		{"商拍大类", Filter{KindGroup: "image"}, "4,6"},
		{"营销大类", Filter{KindGroup: "marketing"}, "1,2,8"},
		{"视频大类", Filter{KindGroup: "video"}, "3,7"},
		{"多标签全部包含", Filter{Tags: []string{"红", "夏"}}, "1"},
		{"单标签", Filter{Tags: []string{"红"}}, "1,2"},
		{"系统标签全部包含", Filter{SysTags: []string{"文生图", "行业:餐饮"}}, "1"},
		{"系统标签+分类", Filter{SysTags: []string{"用途:门头门店"}, Kind: "storefront"}, "1"},
		{"别人的标签不算", Filter{Tags: []string{"不存在"}}, ""},
	}
	for _, c := range checks {
		if got := ids(c.f); got != c.want {
			t.Errorf("%s: got [%s] want [%s]", c.name, got, c.want)
		}
	}

	// 3. 分类计数与列表同口径
	cats, err := r.Asset.Categories(ctx, Filter{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, c := range cats {
		counts[c.Kind.String] += c.N
	}
	for k, n := range map[string]int64{"storefront": 1, "product": 1, "video_final": 1, "model": 2, "": 1, "video_segment": 1, "logo": 1} {
		if counts[k] != n {
			t.Errorf("计数 %q=%d want %d (%v)", k, counts[k], n, counts)
		}
	}
	if _, err := r.Asset.AdminKindStats(ctx, 0, ""); err != nil {
		t.Fatal(err)
	}

	// 4. 系统标签字典：只数本人，按数量倒序
	st, err := r.Asset.SysTagCounts(ctx, 7, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) == 0 || st[0].Name != "文生图" || st[0].N != 2 {
		t.Fatalf("系统标签字典: %+v", st)
	}
	for _, x := range st {
		if x.Name == "用途:门头门店" && x.N != 2 {
			t.Fatalf("用途:门头门店 应为 2（不含别人的）: %+v", st)
		}
	}

	// 5. 批量移除只动本人标签；标签列表按使用次数排序
	if n, err := r.Tag.RemoveLinks(ctx, 7, []int64{1, 2, 9}, []int64{1, 3}); err != nil || n != 2 {
		t.Fatalf("移除 n=%d err=%v（别人的标签 3 不能被移除）", n, err)
	}
	tags, err := r.Tag.List(ctx, 7, TagSortCount)
	if err != nil || len(tags) != 2 || tags[0].Name != "夏" {
		t.Fatalf("按使用次数排序: %+v err=%v", tags, err)
	}
}
