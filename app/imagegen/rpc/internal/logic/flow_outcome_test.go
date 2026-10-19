package logic

import (
	"strings"
	"testing"

	"aigc-local-life/pkg/aigc"
)

// not_found 与「成功却无产物」在日志里必须说清原因，不能只剩一句空的失败。
func TestOutcomeCauseExplainsNonFailedTerminal(t *testing.T) {
	nf := outcomeCause(&aigc.Result{Status: aigc.StatusNotFound})
	if !strings.HasPrefix(nf, "执行面查不到该任务") {
		t.Fatalf("not_found 原因未写明: %s", nf)
	}
	ok := outcomeCause(&aigc.Result{Status: aigc.StatusSuccess})
	if !strings.HasPrefix(ok, "执行面返回成功但没有产物地址") {
		t.Fatalf("success 无产物原因未写明: %s", ok)
	}
	failed := outcomeCause(&aigc.Result{Status: aigc.StatusFailed, ErrorMsg: "boom"})
	if !strings.Contains(failed, `msg="boom"`) {
		t.Fatalf("failed 应带执行面消息: %s", failed)
	}
}
