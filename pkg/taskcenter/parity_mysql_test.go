package taskcenter

// 新旧同步实现的真实 MySQL 对拍 + 锁行为验证。设置 TASKCENTER_PARITY_DSN 才运行（CI 无库时跳过），形如
//
//	TASKCENTER_PARITY_DSN='root:123456@tcp(127.0.0.1:43306)/' go test -p 1 -run 'Parity|Lock' ./pkg/taskcenter/
//
// DSN 不带库名：测试自建一次性库 taskcenter_parity_<随机>_{old,new,lock}，用完 DROP，不碰其它库。
// 表结构取自 Java 迁移（afv_creation_task / afv_creation_task_text / afv_billing_reservation）与 Go_V1.0.19/20
// （afv_exec_submission），只保留同步用到的列与真实的唯一键 / 索引，源表是测试自带的四张表，覆盖各种 Source 配置形态。

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

const parityEnv = "TASKCENTER_PARITY_DSN"

var parityDDL = []string{
	`CREATE TABLE afv_creation_task (
  id BIGINT NOT NULL AUTO_INCREMENT, task_id VARCHAR(64) NOT NULL, user_id BIGINT NOT NULL, type VARCHAR(64) NOT NULL,
  status INT NOT NULL DEFAULT 0, entry_code VARCHAR(64) NOT NULL DEFAULT '', request_params JSON NULL,
  execution_task_id VARCHAR(128) NULL, execution_params JSON NULL, billing_status VARCHAR(32) NULL, price_points BIGINT NULL,
  feature_code VARCHAR(64) NULL, model_id BIGINT NULL, reservation_no VARCHAR(64) NULL, result_url VARCHAR(2048) NULL,
  result_urls JSON NULL, cover_url VARCHAR(2048) NULL, error_msg TEXT NULL, is_transferred TINYINT NOT NULL DEFAULT 0,
  deleted TINYINT NOT NULL DEFAULT 0, origin_service VARCHAR(32) NULL, origin_ref VARCHAR(64) NULL,
  create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id), UNIQUE KEY uk_task_id (task_id), KEY idx_status_update_time (status, update_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE afv_creation_task_text (
  id BIGINT NOT NULL AUTO_INCREMENT, task_id VARCHAR(64) NOT NULL, content MEDIUMTEXT NOT NULL,
  create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id), UNIQUE KEY uk_task_id (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE afv_billing_reservation (
  id BIGINT NOT NULL AUTO_INCREMENT, reservation_no VARCHAR(64) NOT NULL, user_id BIGINT NOT NULL, feature_code VARCHAR(64) NOT NULL,
  model_id BIGINT NULL, price_points BIGINT NOT NULL, biz_type VARCHAR(32) NOT NULL, biz_id VARCHAR(64) NOT NULL,
  attempt_no INT NOT NULL DEFAULT 0, status VARCHAR(32) NOT NULL, idempotency_key VARCHAR(128) NOT NULL,
  deleted TINYINT NOT NULL DEFAULT 0, create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id), UNIQUE KEY uk_reservation_no (reservation_no), UNIQUE KEY uk_idempotency_key (idempotency_key),
  UNIQUE KEY uk_biz (biz_type, biz_id, attempt_no), KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE afv_exec_submission (
  exec_task_id VARCHAR(128) NOT NULL, task_type INT NOT NULL, routed_task_type INT NULL, input JSON NULL, error_msg VARCHAR(512) NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, PRIMARY KEY (exec_task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 源表 A：图片类（JSON 取数、执行面任务号、计费、Kind / Include 筛选）
	`CREATE TABLE tc_img (
  id BIGINT NOT NULL AUTO_INCREMENT, user_id BIGINT NOT NULL, kind VARCHAR(32) NOT NULL, status INT NOT NULL DEFAULT 0,
  image_urls JSON NULL, tasks JSON NULL, fail_reason VARCHAR(512) NULL, content TEXT NULL, request_id VARCHAR(255) NULL,
  size VARCHAR(32) NULL, deleted TINYINT NOT NULL DEFAULT 0, create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 源表 B：文案类（文本产物 + 计费）
	`CREATE TABLE tc_copy (
  id BIGINT NOT NULL AUTO_INCREMENT, user_id BIGINT NOT NULL, title VARCHAR(255) NOT NULL DEFAULT '', topic VARCHAR(512) NULL,
  body TEXT NOT NULL, status INT NOT NULL DEFAULT 0, fail_reason VARCHAR(512) NULL, deleted TINYINT NOT NULL DEFAULT 0,
  create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 源表 C：bun 风格（created_at / updated_at、Kind、Include、执行任务号取列、文本产物 + 计费）
	`CREATE TABLE tc_studio (
  id BIGINT NOT NULL AUTO_INCREMENT, user_id BIGINT NOT NULL, task_id VARCHAR(64) NULL, kind VARCHAR(32) NOT NULL,
  status INT NOT NULL DEFAULT 0, result_url VARCHAR(2048) NULL, error_reason VARCHAR(512) NULL, prompt_text TEXT NULL,
  billing_request_id VARCHAR(128) NULL, deleted TINYINT NOT NULL DEFAULT 0, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 源表 D：无计费、无执行任务号
	`CREATE TABLE tc_note (
  id BIGINT NOT NULL AUTO_INCREMENT, user_id BIGINT NOT NULL, title VARCHAR(255) NULL, cover VARCHAR(512) NULL, status INT NOT NULL DEFAULT 0,
  deleted TINYINT NOT NULL DEFAULT 0, create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// paritySources 对应线上四类配置形态（取自 imagegen / copywriting / studio / note 的真实写法，表名换成测试表）。
func paritySources() []Source {
	return []Source{
		{
			Table: "tc_img", Prefix: "img", Kind: "poster", EntryCode: "poster", Type: "image",
			Include: "status <> 9",
			Status:  "CASE status WHEN 0 THEN 1 WHEN 1 THEN 2 ELSE 3 END",
			Result:  "JSON_UNQUOTE(JSON_EXTRACT(image_urls, '$[0]'))", Results: "image_urls", Fail: "fail_reason", Prompt: "content",
			BillingRequest: "s.`request_id`",
			RequestPairs:   "'prompt', content, 'kind', kind, 'size', size",
			ExecTaskID:     "JSON_UNQUOTE(JSON_EXTRACT(tasks, '$[0].task_id'))",
		},
		{
			Table: "tc_copy", Prefix: "cw", EntryCode: "aiCopywriting", Type: "copywriting",
			Status: "CASE status WHEN 0 THEN 1 WHEN 1 THEN 2 ELSE 3 END", Fail: "fail_reason",
			Prompt: "COALESCE(NULLIF(topic, ''), title)", BillingRequest: "CONCAT('copywriting-', s.id)", ResultText: "s.`body`",
			RequestPairs: "'prompt', COALESCE(NULLIF(topic, ''), title), 'title', title",
		},
		{
			Table: "tc_studio", Prefix: "tyo", Kind: "tryon", EntryCode: "studio_tryon", Type: "image",
			Include: "task_id IS NOT NULL",
			Status:  "CASE status WHEN 2 THEN 2 WHEN 3 THEN 3 WHEN 4 THEN 4 ELSE 1 END",
			Result:  "result_url", Results: "IF(result_url IS NULL, NULL, JSON_ARRAY(result_url))", Fail: "error_reason",
			Prompt: "LEFT(prompt_text, 500)", BillingRequest: "s.`billing_request_id`", ResultText: "prompt_text",
			RequestPairs: "'prompt', LEFT(prompt_text, 500), 'kind', kind", ExecTaskID: "task_id",
			CreateTimeCol: "created_at", UpdateTimeCol: "updated_at",
		},
		{
			Table: "tc_note", Prefix: "note", EntryCode: "note", Type: "image",
			Status: "status", Result: "cover", Cover: "COALESCE(cover, '')", Prompt: "title",
		},
	}
}

type parityEnvT struct {
	admin *sql.DB
	dsn   string
}

func parityDB(t *testing.T) *parityEnvT {
	t.Helper()
	dsn := os.Getenv(parityEnv)
	if dsn == "" {
		t.Skip(parityEnv + " 未设置，跳过真实 MySQL 对拍")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Ping(); err != nil {
		t.Skipf("MySQL 不可用：%v", err)
	}
	t.Cleanup(func() { admin.Close() })
	return &parityEnvT{admin: admin, dsn: dsn}
}

// dsnFor 在 DSN（形如 user:pw@tcp(h:p)/）里填库名并追加参数。
func (e *parityEnvT) dsnFor(schema string, params ...string) string {
	i := strings.Index(e.dsn, ")/")
	base := e.dsn[:i+2] + schema
	rest := e.dsn[i+2:]
	if q := strings.Index(rest, "?"); q >= 0 {
		params = append(strings.Split(rest[q+1:], "&"), params...)
	}
	return base + "?" + strings.Join(append(params, "charset=utf8mb4", "collation=utf8mb4_unicode_ci"), "&")
}

// newSchema 建一次性库与全部表，结束时 DROP。
func (e *parityEnvT) newSchema(t *testing.T, tag string) string {
	t.Helper()
	name := fmt.Sprintf("taskcenter_parity_%06d_%s", rand.Intn(1000000), tag)
	if _, err := e.admin.Exec("CREATE DATABASE `" + name + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.admin.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
	db, err := sql.Open("mysql", e.dsnFor(name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ddl := range parityDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\n%s", err, ddl)
		}
	}
	return name
}

type fx struct {
	t   *testing.T
	db  *sql.DB
	seq *int64     // 预留单序号：两个库各自从 1 开始，保证 reservation_no 一致
	now *time.Time // 本阶段统一的「库当前时间」：两个库用同一个值，时间列才逐位一致
}

func (f fx) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("%v\n%s %v", err, q, args)
	}
}

func (f fx) str(q string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.db.QueryRow(q, args...).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// resv 插一条预留单。upd 为 update_time（显式给，避免 ON UPDATE 带来的时间差）。
func (f fx) resv(user int64, key, status string, price int64, feature string, model any, attempt int, deleted int, upd string) {
	*f.seq++
	n := *f.seq
	f.exec("INSERT INTO afv_billing_reservation (reservation_no, user_id, feature_code, model_id, price_points, biz_type, biz_id, attempt_no, status, "+
		"idempotency_key, deleted, create_time, update_time) VALUES (?,?,?,?,?,'LOCALLIFE',?,?,?,?,?,?,?)",
		fmt.Sprintf("BR-%d", n), user, feature, model, price, key, attempt, status, fmt.Sprintf("idem-%d-%s", n, key), deleted, upd, upd)
}

func (f fx) setResv(key, status, upd string) {
	f.exec("UPDATE afv_billing_reservation SET status=?, update_time=? WHERE biz_type='LOCALLIFE' AND biz_id=? AND deleted=0", status, upd, key)
}

func ts(base time.Time, offset time.Duration) string {
	return base.Add(offset).Format("2006-01-02 15:04:05")
}

func (f fx) dbNow() time.Time { return *f.now }

// readDBNow 读库当前时间（秒）。
func readDBNow(t *testing.T, db *sql.DB) time.Time {
	var s string
	if err := db.QueryRow("SELECT DATE_FORMAT(NOW(), '%Y-%m-%d %H:%i:%s')").Scan(&s); err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

var longReq = strings.Repeat("长请求-abcdefghij-", 8) // 远超 64 字节

// seed0 首轮全量要覆盖的各种形态。
func seed0(f fx) {
	b := f.dbNow()
	old, mid, recent := ts(b, -2*time.Hour), ts(b, -90*time.Minute), ts(b, -20*time.Second)
	ins := func(q string, a ...any) { f.exec(q, a...) }
	// ---- tc_img（poster）----
	img := "INSERT INTO tc_img (id,user_id,kind,status,image_urls,tasks,fail_reason,content,request_id,size,deleted,create_time,update_time) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)"
	ins(img, 1, 7, "poster", 0, nil, nil, nil, `有"引号"与\反斜杠`, "r1", "1:1", 0, old, old)                                              // 无预留单、无执行任务
	ins(img, 2, 7, "poster", 1, `["http://x/2.png","http://x/2b.png"]`, `[{"task_id":"E2"}]`, nil, "p2", "r2", "16:9", 0, old, mid) // 提交快照 + CONFIRMED
	ins(img, 3, 8, "poster", 2, nil, `[{"task_id":"E3"}]`, "boom 失败😀", "p3", "r3", "1:1", 0, old, recent)                           // 失败 + REFUNDED（无快照）
	ins(img, 4, 7, "poster", 9, nil, nil, nil, "p4", "r4", "1:1", 0, old, old)                                                      // Include 排除
	ins(img, 5, 7, "longImage", 1, nil, nil, nil, "p5", "r5", "1:1", 0, old, old)                                                   // Kind 排除
	ins(img, 6, 7, "poster", 1, nil, nil, nil, "p6", "r6", "1:1", 1, old, mid)                                                      // 软删，预留 HELD
	ins(img, 7, 7, "poster", 1, nil, nil, nil, "p7", longReq, "1:1", 0, old, mid)                                                   // 超长键 → SHA-256
	ins(img, 8, 7, "poster", 1, nil, nil, nil, "p8", "r8", "1:1", 0, old, mid)                                                      // 两次尝试
	ins(img, 9, 7, "poster", 1, nil, nil, nil, "p9", "r9", "1:1", 0, old, mid)                                                      // 未知预留状态
	ins(img, 10, 7, "poster", 2, nil, nil, nil, "p10", "r10", "1:1", 0, old, mid)                                                   // EXPIRED
	ins(img, 11, 7, "poster", 2, nil, nil, nil, "p11", "r11", "1:1", 0, old, mid)                                                   // PARTIALLY_REFUNDED
	ins(img, 12, 7, "poster", 1, nil, nil, nil, "p12", "r12", "1:1", 0, old, mid)                                                   // 预留单已软删 → NONE
	ins(img, 13, 7, "poster", 2, `["http://x/13.png"]`, `[{"task_id":"E13"}]`, nil, "p13", "r13", "1:1", 0, old, mid)               // 快照 exec_task_id 大小写不同
	ins(img, 14, 7, "poster", 2, `[]`, `[]`, nil, "p14", nil, nil, 0, old, mid)                                                     // request_id NULL → 计费键 NULL
	ins(img, 15, 7, "poster", 1, nil, `[{"task_id":"E15"}]`, nil, "p15", "r15", "1:1", 0, old, mid)                                 // 快照 input NULL
	f.exec("INSERT INTO afv_exec_submission (exec_task_id, task_type, input) VALUES ('E2',1,'{\"a\":1,\"b\":[1,2]}'),('e13',1,'{\"k\":\"v\"}'),('E15',1,NULL)")
	f.resv(7, "7_r2", "CONFIRMED", 30, "f_img", 5, 0, 0, mid)
	f.resv(8, "8_r3", "REFUNDED", 20, "f_img", 5, 0, 0, recent)
	f.resv(7, "7_r6", "HELD", 10, "f_img", 5, 0, 0, mid)
	f.resv(7, f.str("SELECT LEFT(CONCAT(7,'_',SHA2(?,256)),64)", "7_"+longReq), "CONFIRMED", 77, "f_long", nil, 0, 0, mid)
	f.resv(7, "7_r8", "CANCELLED", 8, "f_a0", 1, 0, 0, mid)
	f.resv(7, "7_r8", "HELD", 9, "f_a1", 2, 1, 0, mid)
	f.resv(7, "7_r9", "WEIRD", 1, "f_img", 5, 0, 0, mid)
	f.resv(7, "7_r10", "EXPIRED", 12, "f_img", 5, 0, 0, mid)
	f.resv(7, "7_r11", "PARTIALLY_REFUNDED", 13, "f_img", 5, 0, 0, mid)
	f.resv(7, "7_r12", "CONFIRMED", 14, "f_img", 5, 0, 1, mid)
	// ---- tc_copy ----
	cp := "INSERT INTO tc_copy (id,user_id,title,topic,body,status,fail_reason,deleted,create_time,update_time) VALUES (?,?,?,?,?,?,?,?,?,?)"
	ins(cp, 1, 7, "标题1", "话题1", "正文一\n第二行", 1, nil, 0, old, mid)
	ins(cp, 2, 7, "标题2", "", "", 1, nil, 0, old, mid) // 空正文：不写文本表
	ins(cp, 3, 7, "标题3", nil, "正文三", 2, "生成失败", 0, old, recent)
	ins(cp, 4, 7, "标题4", "t4", "正文四", 0, nil, 0, old, mid) // 无预留单
	f.resv(7, "7_copywriting-1", "CONFIRMED", 1, "f_text", nil, 0, 0, mid)
	f.resv(7, "7_copywriting-3", "REFUNDED", 1, "f_text", nil, 0, 0, recent)
	// ---- tc_studio ----
	st := "INSERT INTO tc_studio (id,user_id,task_id,kind,status,result_url,error_reason,prompt_text,billing_request_id,deleted,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"
	ins(st, 1, 7, "T1", "tryon", 0, nil, nil, "提示词A", "b1", 0, old, mid)
	ins(st, 2, 7, "T2", "tryon", 2, "http://x/s2.png", nil, "提示词B", "b2", 0, old, mid)
	ins(st, 3, 7, nil, "tryon", 0, nil, nil, "未回填任务号", "b3", 0, old, mid) // Include 排除
	ins(st, 4, 7, "T4", "pose", 2, "http://x/s4.png", nil, "别的 kind", "b4", 0, old, mid)
	ins(st, 5, 7, "T5", "tryon", 4, nil, nil, nil, "b5", 0, old, mid) // 已取消，无提示词
	f.exec("INSERT INTO afv_exec_submission (exec_task_id, task_type, input) VALUES ('T2',2,'{\"s\":2}')")
	f.resv(7, "7_b1", "HELD", 5, "f_st", 9, 0, 0, mid)
	f.resv(7, "7_b2", "CONFIRMED", 6, "f_st", 9, 0, 0, mid)
	f.resv(7, "7_b5", "CANCELLED", 7, "f_st", 9, 0, 0, mid)
	// ---- tc_note ----
	nt := "INSERT INTO tc_note (id,user_id,title,cover,status,deleted,create_time,update_time) VALUES (?,?,?,?,?,?,?,?)"
	ins(nt, 1, 7, "笔记1", "http://x/n1.png", 1, 0, old, mid)
	ins(nt, 2, 7, "笔记2", nil, 2, 0, old, mid)
	ins(nt, 3, 8, nil, "http://x/n3.png", 3, 1, old, recent)
}

// mutate1 第二轮：状态流转、只动预留单、软删、正文变更、补快照、新增行、排除→纳入、目标行被 Java 侧改过。
func mutate1(f fx) {
	b := f.dbNow()
	now := ts(b, 0)
	// 源行状态流转（同时 touch update_time）
	f.exec("UPDATE tc_img SET status=2, image_urls='[\"http://x/1.png\"]', update_time=? WHERE id=1", now)
	f.exec("UPDATE tc_img SET status=2, update_time=? WHERE id=2", now)
	// 只动预留单，源行 update_time 仍是 90 分钟前（OR EXISTS 要覆盖的情形）
	f.setResv("7_r6", "REFUNDED", now)
	f.setResv("7_r9", "CONFIRMED", now)
	f.setResv("7_copywriting-1", "REFUNDED", now)
	f.setResv("7_b1", "CONFIRMED", now)
	// 预留单被软删：下一次同步 legacy 里 r.deleted=0 取不到 → NONE（仅在源行或预留窗口触发时才会变）
	f.exec("UPDATE afv_billing_reservation SET deleted=1, update_time=? WHERE biz_id='7_r11'", now)
	// 软删源行
	f.exec("UPDATE tc_img SET deleted=1, update_time=? WHERE id=7", now)
	// 正文变更 / 失败
	f.exec("UPDATE tc_copy SET body='正文二（补写）', status=1, update_time=? WHERE id=2", now)
	f.exec("UPDATE tc_copy SET body='', update_time=? WHERE id=1", now) // 正文清空：文本表不更新（保持旧值）
	// 新增行 + 补提交快照（源行同时 touch）
	f.exec("INSERT INTO tc_img (id,user_id,kind,status,tasks,content,request_id,size,create_time,update_time) VALUES (16,8,'poster',1,'[{\"task_id\":\"E16\"}]','p16','r16','1:1',?,?)", now, now)
	f.exec("INSERT INTO afv_exec_submission (exec_task_id, task_type, input) VALUES ('E16',3,'{\"n\":16}')")
	f.resv(8, "8_r16", "HELD", 40, "f_img", 5, 0, 0, now)
	// 提交快照晚到、源行没动：legacy / new 都不会补（行为一致）
	f.exec("INSERT INTO afv_exec_submission (exec_task_id, task_type, input) VALUES ('E3',3,'{\"late\":true}')")
	// 排除 → 纳入
	f.exec("UPDATE tc_img SET status=1, update_time=? WHERE id=4", now)
	f.exec("UPDATE tc_studio SET task_id='T3', updated_at=? WHERE id=3", now)
	f.exec("UPDATE tc_studio SET status=3, error_reason='失败', updated_at=? WHERE id=1", now)
	// 笔记
	f.exec("UPDATE tc_note SET status=2, cover='http://x/n1b.png', update_time=? WHERE id=1", now)
	// 目标行被 Java 侧改过：deleted 置 1（GREATEST 要保留）、执行参数预置且无快照的行 COALESCE 保留
	// 这里改的是目标表，调用方对 old / new 两个库都执行同样语句（由 applyTargetTweaks 完成）
}

// applyTargetTweaks 对目标表做 Java 侧的改动（两库一致）。必须在首轮同步之后、第二轮同步之前调用。
func applyTargetTweaks(f fx, now string) {
	ref := func(prefix string, id int64) string { return Ref(prefix, id) }
	f.exec("UPDATE afv_creation_task SET deleted=1 WHERE task_id=?", ref("img", 3))
	f.exec("UPDATE afv_creation_task SET execution_params='{\"java\":\"kept\"}' WHERE task_id=?", ref("img", 15))
	f.exec("UPDATE tc_img SET fail_reason='补一次', update_time=? WHERE id IN (3,15)", now) // 触发这两行重新同步
}

// mutate2 第三轮：再来一批变更，并验证无变化时幂等。
func mutate2(f fx) {
	b := f.dbNow()
	now := ts(b, 0)
	f.exec("UPDATE tc_studio SET status=2, result_url='http://x/s1.png', updated_at=? WHERE id=1", now)
	f.setResv("7_b1", "REFUNDED", now)
	f.setResv("8_r16", "CONFIRMED", now)
	f.exec("UPDATE tc_img SET status=0, update_time=? WHERE id=16", now) // 状态倒退也照搬
	f.exec("INSERT INTO tc_copy (id,user_id,title,topic,body,status,create_time,update_time) VALUES (5,8,'标题5','t5','正文五',1,?,?)", now, now)
	f.resv(8, "8_copywriting-5", "HELD", 2, "f_text", nil, 0, 0, now)
	f.exec("DELETE FROM tc_note WHERE id=2") // 物理删除：两条路径都不处理
}

type tableDump map[string][]string

func dumpTable(t *testing.T, db *sql.DB, table, key string) (cols []string, rows map[string][]string) {
	t.Helper()
	r, err := db.Query("SELECT * FROM " + table + " ORDER BY " + key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cols, _ = r.Columns()
	rows = map[string][]string{}
	kidx := -1
	for i, c := range cols {
		if c == key {
			kidx = i
		}
	}
	for r.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(cols))
		for i, v := range vals {
			if v.Valid {
				out[i] = v.String
			} else {
				out[i] = "<NULL>"
			}
		}
		rows[out[kidx]] = out
	}
	return
}

// compareTargets 逐列比对两个库的目标表；返回比对的行数 / 单元格数与差异。id 是自增主键（顺序可能不同），不比。
func compareTargets(t *testing.T, a, b *sql.DB) (rows, cells int, diffs []string) {
	t.Helper()
	for _, tb := range []string{"afv_creation_task", "afv_creation_task_text"} {
		ca, ra := dumpTable(t, a, tb, "task_id")
		cb, rb := dumpTable(t, b, tb, "task_id")
		if strings.Join(ca, ",") != strings.Join(cb, ",") {
			t.Fatalf("%s 列不一致", tb)
		}
		keys := map[string]bool{}
		for k := range ra {
			keys[k] = true
		}
		for k := range rb {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			x, y := ra[k], rb[k]
			if x == nil || y == nil {
				diffs = append(diffs, fmt.Sprintf("%s %s: 只存在于 %s", tb, k, map[bool]string{true: "old", false: "new"}[x != nil]))
				continue
			}
			rows++
			for i, c := range ca {
				if c == "id" {
					continue
				}
				cells++
				if x[i] != y[i] {
					diffs = append(diffs, fmt.Sprintf("%s %s.%s: old=%q new=%q", tb, k, c, x[i], y[i]))
				}
			}
		}
	}
	return
}

func TestParityLegacyVsNew(t *testing.T) {
	e := parityDB(t)
	ctx := context.Background()
	oldName, newName := e.newSchema(t, "old"), e.newSchema(t, "new")
	oldDB, _ := sql.Open("mysql", e.dsnFor(oldName))
	newDB, _ := sql.Open("mysql", e.dsnFor(newName))
	defer oldDB.Close()
	defer newDB.Close()
	var so, sn int64
	var phase time.Time
	fo, fn := fx{t, oldDB, &so, &phase}, fx{t, newDB, &sn, &phase}
	tick := func() { phase = readDBNow(t, oldDB) }
	oldY := New(sqlx.NewMysql(e.dsnFor(oldName)), 10*time.Minute, paritySources()...).WithMode(ModeLegacy)
	newY := New(sqlx.NewMysql(e.dsnFor(newName)), 10*time.Minute, paritySources()...).WithMode(ModeNew)
	// 小分页逼出多页读取路径
	newY.WithSyncLimits(4, 3, 3, 0)

	totalRows, totalCells := 0, 0
	round := func(name string) {
		t.Helper()
		if err := oldY.Sync(ctx); err != nil {
			t.Fatalf("%s legacy: %v", name, err)
		}
		// 首轮分页 3 页 × 4 行读不完时分多轮追平（catch-up），直到稳态
		for i := 0; i < 20; i++ {
			if err := newY.Sync(ctx); err != nil {
				t.Fatalf("%s new: %v", name, err)
			}
			if newY.nw.cur[0].st == stSteady && newY.nw.cur[1].st == stSteady && newY.nw.cur[2].st == stSteady && newY.nw.cur[3].st == stSteady {
				break
			}
		}
		r, c, diffs := compareTargets(t, oldDB, newDB)
		totalRows += r
		totalCells += c
		t.Logf("%s：比对 %d 行 / %d 个单元格，差异 %d", name, r, c, len(diffs))
		for _, d := range diffs {
			t.Errorf("%s 差异: %s", name, d)
		}
	}

	tick()
	seed0(fo)
	seed0(fn)
	round("首轮全量")

	time.Sleep(1100 * time.Millisecond)
	tick()
	mutate1(fo)
	mutate1(fn)
	nowS := ts(phase, 0)
	applyTargetTweaks(fo, nowS)
	applyTargetTweaks(fn, nowS)
	round("第二轮（流转/只动预留单/软删/正文/快照/Java 改目标）")

	round("第三轮（无变化，幂等）")

	time.Sleep(1100 * time.Millisecond)
	tick()
	mutate2(fo)
	mutate2(fn)
	round("第四轮（再变更 + 物理删除）")

	// 抽样确认关键语义确实落在库里（防止「两边都一样地错」）
	check := func(db *sql.DB, prefix string, id int64, col, want string) {
		var got sql.NullString
		if err := db.QueryRow("SELECT CAST("+col+" AS CHAR) FROM afv_creation_task WHERE task_id=?", Ref(prefix, id)).Scan(&got); err != nil {
			t.Fatalf("%s:%d %s: %v", prefix, id, col, err)
		}
		if g := got.String; !got.Valid {
			g = "<NULL>"
		} else if g != want {
			t.Errorf("%s:%d %s=%q want %q", prefix, id, col, g, want)
		}
		if !got.Valid && want != "<NULL>" {
			t.Errorf("%s:%d %s=NULL want %q", prefix, id, col, want)
		}
	}
	for _, db := range []*sql.DB{oldDB, newDB} {
		check(db, "img", 1, "billing_status", "NONE")
		check(db, "img", 6, "billing_status", "REFUNDED")  // 只动预留单：HELD → REFUNDED
		check(db, "img", 9, "billing_status", "CONFIRMED") // 未知 → CONFIRMED
		check(db, "img", 6, "deleted", "1")
		check(db, "img", 3, "deleted", "1") // Java 侧置删后 GREATEST 保留
		check(db, "img", 15, "execution_params", `{"java": "kept"}`)
		check(db, "img", 7, "billing_status", "CONFIRMED") // 超长键走 SHA-256
		check(db, "img", 7, "price_points", "77")
		check(db, "cw", 1, "billing_status", "REFUNDED")
		check(db, "tyo", 1, "billing_status", "REFUNDED")
		check(db, "img", 4, "status", "2") // 排除 → 纳入后 status=1 → CASE 1 → 2
	}
	var multiAttempt string
	_ = oldDB.QueryRow("SELECT feature_code FROM afv_creation_task WHERE task_id=?", Ref("img", 8)).Scan(&multiAttempt)
	t.Logf("两次尝试的预留单（attempt 0=CANCELLED / 1=HELD）旧路径取到 feature_code=%s", multiAttempt)
	t.Logf("对拍合计：%d 行（累计各轮）/ %d 个单元格", totalRows, totalCells)

	// shadow：在 legacy 写过的库上跑 shadow，应当零不一致；篡改一个旧路径不会覆盖的列后应当报出
	sh := New(sqlx.NewMysql(e.dsnFor(oldName)), 10*time.Minute, paritySources()...).WithMode(ModeShadow)
	var diffTask string
	sh.OnShadowDiff(func(id string, d []string) { diffTask = id + " " + strings.Join(d, ";") })
	for i := 0; i < 2; i++ {
		if err := sh.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st := sh.ShadowStats()
	t.Logf("shadow（无篡改）：%+v", st)
	if st.Persistent != 0 || st.Suspected != 0 {
		t.Errorf("shadow 误报: %+v %s", st, diffTask)
	}
	if st.Compared == 0 {
		t.Error("shadow 没有比对任何行")
	}
	// 把一行的 type 改掉（不在 ON DUPLICATE KEY UPDATE 列里，旧路径不会纠正）
	tick()
	fo.exec("UPDATE tc_img SET update_time=? WHERE id=16", ts(phase, 0))
	fo.exec("UPDATE afv_creation_task SET type='tampered' WHERE task_id=?", Ref("img", 16))
	for i := 0; i < 3; i++ {
		if err := sh.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st = sh.ShadowStats()
	t.Logf("shadow（篡改后）：%+v 差异=%s", st, diffTask)
	if st.Persistent == 0 || !strings.Contains(diffTask, "type") {
		t.Errorf("shadow 没有报出被篡改的列: %+v %q", st, diffTask)
	}
}
