package taskcenter

import (
	"regexp"
	"strings"
	"testing"
)

// 本地生活任务号：AiCreation_LF_ + 32 位小写十六进制。
var taskIDShape = regexp.MustCompile(`^AiCreation_LF_[0-9a-f]{32}$`)

// 格式与取值锁死：md5("img:25")，换算法或改前缀都会在这里报错。
func TestRefIsUUIDShapedAndStable(t *testing.T) {
	got := Ref("img", 25)
	if got != "AiCreation_LF_78d041880cfc68974695eda3e14df316" {
		t.Fatalf("Ref(img,25)=%s", got)
	}
	if !taskIDShape.MatchString(got) {
		t.Fatalf("不是 AiCreation_LF_+32hex 格式: %s", got)
	}
}

// 同步语句里的 MySQL 表达式必须算的是同一个键：同一前缀、同一主键，两边 md5 的输入一致。
func TestRefSQLUsesSameKeyAsRef(t *testing.T) {
	s := Source{Prefix: "vid"}
	if !strings.Contains(s.refSQL(), "MD5(CONCAT('vid:', id))") {
		t.Fatalf("refSQL 的键与 Ref 不一致: %s", s.refSQL())
	}
	if Ref("vid", 28) == Ref("img", 28) {
		t.Fatal("不同前缀不能得到同一个编号")
	}
}

// 同步语句必须同时写入入参快照、执行任务号、执行参数（与 Java 的 requestParams / executionTaskId / executionParams 对齐）。
func TestSyncSQLWritesRequestAndExecutionFields(t *testing.T) {
	s := Source{
		Table: "afv_video_creation", Prefix: "vid", EntryCode: "shortDrama", Type: "video",
		Status: "1", Prompt: "user_text",
		RequestPairs: "'duration', duration, 'model', model",
		ExecTaskID:   "gen_task_id",
		ExecParams:   "JSON_OBJECT('model', model)",
	}
	q := s.sql()
	for _, want := range []string{
		"request_params", "execution_task_id", "execution_params",
		"JSON_OBJECT('entryCode', 'shortDrama', 'duration', duration, 'model', model)",
		"gen_task_id", "JSON_OBJECT('model', model)",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("同步语句缺少 %q", want)
		}
	}
	// 没配 RequestPairs 的源保持旧行为：只有 entryCode 与 prompt
	old := Source{Table: "t", Prefix: "x", EntryCode: "e", Type: "image", Status: "1", Prompt: "content"}
	if got := old.requestSQL(); got != "JSON_OBJECT('entryCode', 'e', 'prompt', content)" {
		t.Fatalf("旧行为变了: %s", got)
	}
}

// 占位符数量必须与参数一致，否则整条同步语句报错、该源全部不同步。
func TestSyncArgsMatchPlaceholders(t *testing.T) {
	withBilling := Source{Table: "t", Prefix: "img", EntryCode: "poster", Type: "image", Status: "1", BillingRequest: "request_id"}
	if got, want := len(withBilling.args(60)), strings.Count(withBilling.sql(), "?"); got != want {
		t.Fatalf("有计费：参数 %d 个，占位符 %d 个", got, want)
	}
	noBilling := Source{Table: "t", Prefix: "img", EntryCode: "poster", Type: "image", Status: "1"}
	if got, want := len(noBilling.args(60)), strings.Count(noBilling.sql(), "?"); got != want {
		t.Fatalf("无计费：参数 %d 个，占位符 %d 个", got, want)
	}
}

// 关联键与 pkg/billing 的 bizID 一致：超过 64 字节要走 SHA-256 截断，且必须包含 LOCALLIFE 业务类型。
func TestBillingKeyMatchesBillingRule(t *testing.T) {
	s := Source{Table: "t", Prefix: "vid", BillingRequest: "gen_task_id"}
	q := s.sql()
	for _, want := range []string{"biz_type = 'LOCALLIFE'", "SHA2(", "LENGTH(", "<= 64"} {
		if !strings.Contains(q, want) {
			t.Fatalf("计费关联缺少 %q", want)
		}
	}
	if !strings.Contains(q, "'NONE'") && !strings.Contains(q, "IFNULL(") {
		t.Fatalf("没有预留单时应显示 NONE")
	}
}

func TestOriginRef(t *testing.T) {
	if got := OriginRef("img", 123); got != "img:123" {
		t.Fatalf("OriginRef = %q, want img:123", got)
	}
	s := Source{Prefix: "dh"}
	if got := s.originRefSQL(); got != "CONCAT('dh:', s.id)" {
		t.Fatalf("originRefSQL = %q", got)
	}
}
