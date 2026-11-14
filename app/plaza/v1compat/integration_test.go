package v1compat

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// 真库集成测试：验证 SQL 口径（enabled / deleted 过滤、排序、头条日期窗口、复刻统计的两个窗口）。
// 做法：把 fixture 的 main 数据集灌进临时库，同一时刻分别用 SQLSource（真库）与内存 fakeSource 跑全部用例，
// 两边输出必须一致（payload 子树除外：MySQL 的 JSON 列会把超出 uint64 的整数存成 double、重排键序，
// payload 的逐字节口径已由 golden_test 对 Java 覆盖）。
//
//	mysql -uroot -p123456 -h127.0.0.1 -P43306 -e 'CREATE DATABASE plaza_v1_scratch DEFAULT CHARSET utf8mb4'
//	PLAZA_V1_TEST_DSN='root:123456@tcp(127.0.0.1:43306)/plaza_v1_scratch?charset=utf8mb4' go test ./app/plaza/v1compat/ -run Integration
//	mysql -uroot -p123456 -h127.0.0.1 -P43306 -e 'DROP DATABASE plaza_v1_scratch'
//
// 没设 PLAZA_V1_TEST_DSN 时跳过；库名必须含 scratch（会建表、删表）。
func TestIntegrationSQLMatchesFake(t *testing.T) {
	dsn := os.Getenv("PLAZA_V1_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PLAZA_V1_TEST_DSN，跳过真库集成测试")
	}
	if !strings.Contains(dsn, "scratch") {
		t.Fatalf("PLAZA_V1_TEST_DSN 必须指向临时库（库名含 scratch）: %s", dsn)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	drop := func() {
		for _, tb := range []string{"afv_compat_plaza_template", "afv_plaza_headline", "afv_agent_turn"} {
			_, _ = db.Exec("DROP TABLE IF EXISTS " + tb)
		}
	}
	drop()
	defer drop()
	// 表结构照 Java Flyway V1.0.186 / V1.0.217（afv_agent_turn 只建统计用到的列）
	exec(`CREATE TABLE afv_compat_plaza_template (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, category VARCHAR(64) NULL, title VARCHAR(255) NOT NULL,
		description VARCHAR(1000) NULL, tags_json JSON NULL, data_json JSON NOT NULL, sort_order INT NOT NULL DEFAULT 0,
		enabled TINYINT NOT NULL DEFAULT 1, create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, deleted TINYINT NOT NULL DEFAULT 0
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)
	exec(`CREATE TABLE afv_plaza_headline (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, plaza_template_id BIGINT NOT NULL, start_date DATE NOT NULL,
		end_date DATE NOT NULL, why VARCHAR(60) NOT NULL, sort_order INT NOT NULL DEFAULT 0, enabled TINYINT NOT NULL DEFAULT 1,
		create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted TINYINT NOT NULL DEFAULT 0
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)
	exec(`CREATE TABLE afv_agent_turn (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, plaza_template_id BIGINT NULL,
		create_time DATETIME NOT NULL, KEY idx (plaza_template_id, create_time))`)

	var fx struct {
		Datasets []fixtureDataset `json:"datasets"`
	}
	loadJSON(t, "testdata/fixture.json", &fx)
	ds := fx.Datasets[0]
	if ds.Name != "main" {
		t.Fatal("fixture 第一个数据集应是 main")
	}
	now := time.Now().In(shanghai)
	today := now.Format("2006-01-02")
	day := func(off int) string { return now.AddDate(0, 0, off).Format("2006-01-02") }
	for _, tp := range ds.Templates {
		exec("INSERT INTO afv_compat_plaza_template (id, title, description, tags_json, data_json, sort_order, enabled, update_time) VALUES (?,?,?,?,?,?,?,?)",
			tp.ID, *tp.Title, tp.Description, tp.TagsJSON, *tp.DataJSON, tp.SortOrder, tp.Enabled, tp.UpdateTime)
	}
	// 逻辑删除的模板与头条：两边都不得出现
	exec(`INSERT INTO afv_compat_plaza_template (id, title, tags_json, data_json, sort_order, enabled, deleted) VALUES (99, '已删除', '{"tags":["删除标签"]}', '{}', -9, 1, 1)`)
	for _, h := range ds.Headlines {
		exec("INSERT INTO afv_plaza_headline (id, plaza_template_id, start_date, end_date, why, sort_order, enabled) VALUES (?,?,?,?,?,?,?)",
			h.ID, h.PlazaTemplateID, day(h.StartOffset), day(h.EndOffset), *h.Why, h.SortOrder, h.Enabled)
	}
	exec("INSERT INTO afv_plaza_headline (id, plaza_template_id, start_date, end_date, why, enabled, deleted) VALUES (98, 99, ?, ?, '删除的头条', 1, 1)", today, today)
	exec("INSERT INTO afv_plaza_headline (id, plaza_template_id, start_date, end_date, why, enabled, deleted) VALUES (97, 4, ?, ?, '删除的头条', 1, 1)", today, today)
	// 复刻统计：本窗口 [now-7d, now) 与上一窗口 [now-14d, now-7d)；窗口外与 NULL 的不计
	ts := func(d time.Duration) string { return now.Add(d).Format("2006-01-02 15:04:05") }
	for _, u := range ds.Usage {
		for i := int64(0); i < u.Uses; i++ {
			exec("INSERT INTO afv_agent_turn (plaza_template_id, create_time) VALUES (?, ?)", u.TemplateID, ts(-time.Hour))
		}
		for i := int64(0); i < u.PreviousUses; i++ {
			exec("INSERT INTO afv_agent_turn (plaza_template_id, create_time) VALUES (?, ?)", u.TemplateID, ts(-8*24*time.Hour))
		}
		exec("INSERT INTO afv_agent_turn (plaza_template_id, create_time) VALUES (?, ?)", u.TemplateID, ts(-15*24*time.Hour))
		exec("INSERT INTO afv_agent_turn (plaza_template_id, create_time) VALUES (?, ?)", u.TemplateID, ts(time.Hour))
	}
	exec("INSERT INTO afv_agent_turn (plaza_template_id, create_time) VALUES (NULL, ?)", ts(-time.Hour))

	fixed := func() time.Time { return now }
	real := New(SQLSource{DB: db}, fixed)
	fake := New(&fakeSource{ds: ds}, fixed)
	for _, c := range ds.Cases {
		gotR, fR := run(real, c)
		gotF, fF := run(fake, c)
		if (fR == nil) != (fF == nil) || (fR != nil && fR.Code != fF.Code) {
			t.Fatalf("%s: 失败口径不同 real=%v fake=%v", c.Name, fR, fF)
		}
		if fR != nil {
			continue
		}
		if a, b := withoutPayload(t, gotR), withoutPayload(t, gotF); a != b {
			t.Fatalf("%s: 真库与内存口径不一致\nreal: %s\nfake: %s", c.Name, a, b)
		}
	}
	// 时区 / 窗口：statsFrom = 今天 - 7 天
	data, _ := real.Headlines(t.Context(), "", "", false, false)
	if !strings.Contains(string(data), fmt.Sprintf(`"statsFrom":"%s","statsTo":"%s"`, day(-7), today)) {
		t.Fatalf("统计窗口: %s", data)
	}
}

func withoutPayload(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var strip func(any)
	strip = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			delete(x, "payload")
			for _, e := range x {
				strip(e)
			}
		case []any:
			for _, e := range x {
				strip(e)
			}
		}
	}
	strip(v)
	out, _ := json.Marshal(v)
	return string(out)
}
