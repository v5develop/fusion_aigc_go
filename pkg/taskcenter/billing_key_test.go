package taskcenter

import (
	"strings"
	"testing"
)

// BillingRequest 是 SQL 表达式：列名与 CONCAT 表达式都要原样进入预留单的关联键。
func TestBillingKeySQLUsesRequestExpression(t *testing.T) {
	col := Source{BillingRequest: "s.`request_id`"}
	if got := col.billingKeySQL(); !strings.Contains(got, "CONCAT(s.user_id, '_', s.`request_id`)") {
		t.Fatalf("column expr: %s", got)
	}

	cw := Source{BillingRequest: "CONCAT('copywriting-', s.id)"}
	if got := cw.billingKeySQL(); !strings.Contains(got, "CONCAT(s.user_id, '_', CONCAT('copywriting-', s.id))") {
		t.Fatalf("copywriting expr: %s", got)
	}
}

// 正文走扩展表：textSQL 的占位符个数必须与 sql 相同（共用 args），且只写非空正文，不碰主表的 result 列。
func TestTextSQLMatchesPlaceholdersAndTable(t *testing.T) {
	src := Source{
		Table: "afv_copywriting", Prefix: "cw", EntryCode: "aiCopywriting", Type: "copywriting",
		Status: "CASE status WHEN 0 THEN 1 ELSE 2 END", ResultText: "s.`body`",
		BillingRequest: "CONCAT('copywriting-', s.id)",
	}
	text := src.textSQL()
	if !strings.Contains(text, "INSERT INTO "+TextTable+" (task_id, content") {
		t.Fatalf("table: %s", text)
	}
	if strings.Contains(text, "result_text") || strings.Contains(src.sql(), "result_text") {
		t.Fatalf("main table must not carry body")
	}
	if strings.Count(text, "?") != strings.Count(src.sql(), "?") || len(src.args(60)) != strings.Count(text, "?") {
		t.Fatalf("placeholder mismatch: text=%d sql=%d args=%d", strings.Count(text, "?"), strings.Count(src.sql(), "?"), len(src.args(60)))
	}
}
