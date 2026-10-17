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
