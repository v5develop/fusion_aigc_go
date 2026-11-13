package sharedtables

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoSharedTableWrites 仓库守卫：任何非属主包写 Java 共享表（又不在 allowlist.txt）就失败。
// `go test ./...`（make test）与 `make check-shared-tables` 都会跑到它。
func TestRepoSharedTableWrites(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := LoadAllowlist(filepath.Join(root, "tools", "sharedtables", "allowlist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	viol, stale, err := Scan(root, allow)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range stale {
		t.Errorf("allowlist.txt 里的例外已不再需要，请删掉这一行：%s|%s（理由原文：%s）", a.File, a.Table, a.Reason)
	}
	if len(viol) > 0 {
		var b strings.Builder
		b.WriteString("发现越权写入 Java 共享表。共享表只能由属主包写：\n" +
			"  afv_billing_* -> pkg/billing；afv_creation_task / afv_creation_task_text / afv_exec_submission -> pkg/taskcenter；\n" +
			"  afv_asset / afv_asset_item -> pkg/asset；afv_task_event -> pkg/taskevent。\n" +
			"请改为调用属主包的方法；确有正当理由的在 tools/sharedtables/allowlist.txt 登记「文件|表|理由」。\n违规位置：\n")
		for _, v := range viol {
			b.WriteString("  " + v.String() + "\n")
		}
		t.Error(b.String())
	}
}

func write(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanner(t *testing.T) {
	root := t.TempDir()
	// 属主包：放行
	write(t, root, "pkg/billing/a.go", "package billing\nconst q = \"UPDATE `afv_billing_reservation` SET status = 1\"\n")
	// 越权：SQL 字符串（含拼接 / 变量拼接）
	write(t, root, "app/x/a.go", "package x\nvar cols = \"id\"\nconst a = \"INSERT INTO afv_asset (id) \" +\n\t\"VALUES (1)\"\nvar b = \"DELETE FROM `afv_task_event` WHERE id=\" + cols\n")
	// 越权：bun 链、bun model、bundb 助手
	write(t, root, "app/y/a.go", `package y
import "github.com/uptrace/bun"
type M struct {
	bun.BaseModel `+"`bun:\"table:afv_creation_task,alias:t\"`"+`
	ID int64
}
func f(db *bun.DB) {
	db.NewUpdate().Model((*M)(nil)).Set("a = 1").Exec(nil)
	db.NewInsert().TableExpr("afv_exec_submission").Exec(nil)
	db.NewSelect().TableExpr("afv_asset AS a").Scan(nil)
}
`)
	// 只读不报：SELECT / FOR UPDATE / 旁表 / 注释
	write(t, root, "app/z/a.go", "package z\n// UPDATE afv_asset\nconst r = \"SELECT * FROM afv_asset a JOIN afv_asset_ext e ON 1 FOR UPDATE\"\nconst w = \"UPDATE afv_asset_ext SET a=1\"\n")
	// 测试文件不扫
	write(t, root, "app/z/a_test.go", "package z\nconst t = \"DELETE FROM afv_asset\"\n")
	// 常量表名
	write(t, root, "app/v/a.go", "package v\nconst TableAsset = \"afv_asset\"\nfunc f(db *bun.DB) { db.NewUpdate().Table(TableAsset).Exec(nil) }\n")
	// 例外
	write(t, root, "app/w/a.go", "package w\nconst q = \"UPDATE afv_asset SET name = ?\"\n")

	viol, stale, err := Scan(root, []Allow{{"app/w/a.go", "afv_asset", "测试"}, {"app/none.go", "afv_asset", "过期"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range viol {
		got[v.File+"|"+v.Table] = true
	}
	for _, want := range []string{
		"app/x/a.go|afv_asset", "app/x/a.go|afv_task_event",
		"app/y/a.go|afv_creation_task", "app/y/a.go|afv_exec_submission", "app/v/a.go|afv_asset",
	} {
		if !got[want] {
			t.Errorf("漏报 %s；实际：%v", want, viol)
		}
	}
	for _, v := range viol {
		if strings.HasPrefix(v.File, "pkg/billing") || strings.HasPrefix(v.File, "app/z") || v.File == "app/w/a.go" {
			t.Errorf("误报：%s", v)
		}
	}
	if len(viol) != 5 {
		t.Errorf("违规数应为 5，实际 %d：%v", len(viol), viol)
	}
	if len(stale) != 1 || stale[0].File != "app/none.go" {
		t.Errorf("应报出过期例外：%v", stale)
	}
}

func TestLoadAllowlistRejectsMissingReason(t *testing.T) {
	p := filepath.Join(t.TempDir(), "allowlist.txt")
	if err := os.WriteFile(p, []byte("# c\napp/a.go|afv_asset|\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAllowlist(p); err == nil {
		t.Fatal("理由为空应报错")
	}
}
