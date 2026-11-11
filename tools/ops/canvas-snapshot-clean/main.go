// canvas-snapshot-clean 清理画布快照里被 jsoncase 中间件补出来的下划线副本。
//
// 背景：9bfa445 之前，pkg/jsoncase 改写请求体时会递归下钻自由格式字段，把快照节点 data 里白名单内的 camel 键
// （runId / requestId / taskId …）补出 run_id / request_id / task_id 副本并随保存落库。快照内部的字段以前端原样的
// camelCase 为准（api-canvas.md 1.4），服务端保存时只剥离 camel 运行态键，副本从此留在库里、不再更新，值可能已陈旧。
//
// 规则（cleanSnapshot）：遍历整棵 JSON 树，某个对象里的下划线键只有同时满足以下条件才删：
//   - 在 jsoncase 白名单（pkg/jsoncase known_gen.go）里；
//   - 同一对象里还有它的 camel 孪生键（toCamel 与中间件同一函数），且该 camel 键 toSnake 后正好是它；
//
// 没有 camel 孪生的下划线键一律不动；保留 camel 键（前端读的那个）。
//
// 安全：
//   - 默认 dry-run，只打印每行要删的 JSON 路径与值是否不同；-apply 才写库。
//   - -apply 时先把整批原快照写进备份文件（JSON Lines，O_EXCL 新建并 fsync）再写回；备份失败整批不写并中止整次运行。
//   - 写回带乐观锁：画布 WHERE id=? AND revision=? AND updated_at=?；模板 WHERE id=? AND updated_at=?；行在扫描后被保存 / 删除就记 conflict 跳过（重跑即可）。updated_at 写回原值，不把画布顶到列表最前。
//   - 连接字符集强制 utf8mb4（忽略 DSN 的 charset / collation），快照文本读写不丢字符。
//   - 数字按原文保留（UseNumber，大整数不丢精度），不做 HTML 转义；只改有副本的行。
//   - 幂等：清理后的快照没有副本，重跑不会再改。
//
// 用法（详见同目录 README.md）：
//
//	go run ./tools/ops/canvas-snapshot-clean -dsn 'user:pass@tcp(host:3306)/fusion_aigc'                 # dry-run
//	go run ./tools/ops/canvas-snapshot-clean -dsn '…' -apply -backup /data/backup/canvas-snap.jsonl      # 真写
//
// 退出码：有 error / conflict / bad_json 行为 1，否则 0。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// normalizeDSN 解析 DSN 并强制连接字符集为 utf8mb4：快照按 CAST(snapshot AS CHAR) 读成文本、改完再 CAST(? AS JSON) 写回，
// 连接字符集若被 DSN 设成 latin1 / utf8(mb3)，emoji 等字符会在读出时变成 ?，写回就是永久损坏。
func normalizeDSN(dsn string) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if err := cfg.Apply(mysql.Charset("utf8mb4", "utf8mb4_general_ci")); err != nil { // 覆盖 DSN 里的 charset= / collation=
		return nil, err
	}
	return cfg, nil
}

func main() {
	dsn := flag.String("dsn", os.Getenv("CANVAS_CLEAN_DSN"), "MySQL DSN（默认读环境变量 CANVAS_CLEAN_DSN），如 user:pass@tcp(host:3306)/fusion_aigc")
	tables := flag.String("tables", "canvas,template", "要扫的表：canvas（afv_canvas）、template（afv_canvas_template），逗号分隔")
	batch := flag.Int("batch", 200, "每批读多少行（按主键 keyset 分页）")
	limit := flag.Int("limit", 0, "每张表最多扫多少行（0 = 不限）")
	since := flag.String("since", "", "只扫 updated_at >= 这个时间的行，格式 2006-01-02 15:04:05（空 = 全表）")
	includeDeleted := flag.Bool("include-deleted", false, "连逻辑删除的行一起清理")
	apply := flag.Bool("apply", false, "真写库（默认 dry-run 只打印）")
	backup := flag.String("backup", "", "apply 时的备份文件（JSON Lines，原快照；必须是不存在的新文件）。默认 ./canvas-snapshot-backup-<时间>.jsonl")
	bump := flag.Bool("bump-revision", false, "清理画布时顺带换 revision：正开着这张画布的编辑器下次保存会撞版本冲突、提示重新载入（默认不换，打开中的编辑器可能把带副本的旧快照存回去，重跑即可）")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dsn（或环境变量 CANVAS_CLEAN_DSN）")
		flag.Usage()
		os.Exit(2)
	}
	if *batch <= 0 || *batch > 5000 {
		fmt.Fprintln(os.Stderr, "-batch 取值 1–5000")
		os.Exit(2)
	}
	if *since != "" {
		if _, err := time.Parse(time.DateTime, *since); err != nil {
			fmt.Fprintf(os.Stderr, "-since 格式不对: %v\n", err)
			os.Exit(2)
		}
	}
	ts, err := pickTargets(*tables)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	o := options{Targets: ts, Scan: scanOpts{Since: *since, IncludeDeleted: *includeDeleted, Batch: *batch},
		Limit: *limit, Apply: *apply, BumpRevision: *bump}
	if *apply {
		path := *backup
		if path == "" {
			path = "canvas-snapshot-backup-" + time.Now().Format("20060102-150405") + ".jsonl"
		}
		f, err := openBackup(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "建备份文件失败（不会覆盖已有文件）: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		o.Backup, o.Sync = f, f.Sync
		fmt.Printf("备份写入 %s\n", path)
	}

	cfg, err := normalizeDSN(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DSN 不合法: %v\n", err) // 驱动的 DSN 解析错误不含密码
		os.Exit(2)
	}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连库失败: %v\n", err)
		os.Exit(1)
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	db.SetMaxOpenConns(2)
	s, err := run(context.Background(), &store{db: db}, o, os.Stdout)
	printSummary(os.Stdout, s, *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "中断: %v\n", err)
		os.Exit(1)
	}
	if s.ByResult[resError]+s.ByResult[resConflict]+s.ByResult[resBadJSON] > 0 {
		os.Exit(1)
	}
}

func pickTargets(list string) ([]target, error) {
	var out []target
	seen := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		found := false
		for _, t := range targets {
			if t.Name == name {
				out, found = append(out, t), true
			}
		}
		if !found {
			return nil, fmt.Errorf("未知的表 %q（可选 canvas / template）", name)
		}
		seen[name] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-tables 为空")
	}
	return out, nil
}
