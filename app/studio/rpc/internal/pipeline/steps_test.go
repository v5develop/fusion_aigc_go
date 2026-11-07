package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/aigateway"
)

func TestCacheKeys(t *testing.T) {
	sha := SHA1Hex([]byte("abc"))
	if sha != "a9993e364706816aba3e25717850c26c9cd0d89d" || SHA1String("abc") != sha {
		t.Fatal("sha1")
	}
	if AnalyzeKey(strings.ToUpper(sha)) != sha || SourceKey(sha) != sha || PromptKey(sha, "  ", nil) != sha {
		t.Fatal("plain keys")
	}
	k := PromptKey(sha, "抬头", nil)
	if !strings.HasPrefix(k, sha+":") || len(k) != 57 || k == PromptKey(sha, "低头", nil) {
		t.Fatalf("prompt key = %s", k)
	}
	if CropPlanKey(0) != "batch-0" || len(k) > 80 {
		t.Fatal("crop key")
	}
}

// 多人配对进缓存键：反转配对不能命中默认配对 / 原配对的缓存；空 mapping 与旧键一致。
func TestPromptKeyMapping(t *testing.T) {
	sha := SHA1Hex([]byte("pose"))
	base := PromptKey(sha, "抬头", nil)
	if PromptKey(sha, "抬头", []int{}) != base {
		t.Fatal("empty mapping must keep the old key")
	}
	fwd, rev := PromptKey(sha, "抬头", []int{0, 1}), PromptKey(sha, "抬头", []int{1, 0})
	if fwd == base || rev == base || fwd == rev {
		t.Fatalf("mapping must change key: base=%s fwd=%s rev=%s", base, fwd, rev)
	}
	if rev != base+":m1.0" {
		t.Fatalf("rev = %s", rev)
	}
	// -1 与多位下标不能拼出歧义（[1,-1] ≠ [-1,1]），最长 4 位仍 ≤80
	if PromptKey(sha, "", []int{1, -1}) == PromptKey(sha, "", []int{-1, 1}) {
		t.Fatal("ambiguous mapping key")
	}
	if k := PromptKey(sha, "抬头", []int{-1, -1, -1, -1}); len(k) > 80 {
		t.Fatalf("key too long: %d", len(k))
	}
}

func TestRetryPolicy(t *testing.T) {
	want := []time.Duration{1200 * time.Millisecond, 2400 * time.Millisecond, 4800 * time.Millisecond}
	for i, w := range want {
		if d, ok := GenerateRetryDelay(i); !ok || d != w {
			t.Fatalf("attempt %d = %v", i, d)
		}
	}
	if _, ok := GenerateRetryDelay(3); ok {
		t.Fatal("only 3 retries")
	}
	if CropTimeout != 120*time.Second || AnalyzeTimeout != 120*time.Second || GenerateTimeout != 300*time.Second {
		t.Fatal("timeouts")
	}
	yes := []error{&aigateway.Error{HTTPStatus: 429}, &aigateway.Error{HTTPStatus: 503}, &aigateway.Error{Code: aigateway.CodeTransport}, context.DeadlineExceeded}
	no := []error{nil, &aigateway.Error{HTTPStatus: 409, Code: aigateway.CodeIdempotencyConflict}, &aigateway.Error{HTTPStatus: 400}, &aigateway.Error{HTTPStatus: 401}, context.Canceled, errors.New("x")}
	for _, e := range yes {
		if !RetryableSubmitErr(e) {
			t.Errorf("want retryable %v", e)
		}
	}
	for _, e := range no {
		if RetryableSubmitErr(e) {
			t.Errorf("want not retryable %v", e)
		}
	}
	failed := func(class string, safe bool) *aigateway.Call {
		return &aigateway.Call{Status: aigateway.StatusFailed, RetrySafe: safe, Failure: &aigateway.Failure{Class: class}}
	}
	if !RetryableCall(failed("VENDOR_FAILED", true)) || !RetryableCall(failed("DEADLINE_EXCEEDED", true)) || !RetryableCall(failed("NO_CAPACITY", true)) {
		t.Fatal("retryable classes")
	}
	if RetryableCall(failed("CONTENT_POLICY", true)) || RetryableCall(failed("VENDOR_FAILED", false)) || RetryableCall(&aigateway.Call{Status: aigateway.StatusUnknown}) || RetryableCall(nil) {
		t.Fatal("non-retryable")
	}
}

func TestPlanRun(t *testing.T) {
	var seen []StepLog
	boom := errors.New("boom")
	p := Plan{JobID: "42", OnLog: func(l StepLog) { seen = append(seen, l) }, Steps: []Step{
		StepFunc{StepName: StepCropPlan, Fn: func(context.Context) (StepLog, error) {
			time.Sleep(2 * time.Millisecond)
			return StepLog{Images: 9, Crops: 20}, nil
		}},
		StepFunc{StepName: StepAnalyze, Fn: func(context.Context) (StepLog, error) {
			return StepLog{Degraded: true}, boom
		}},
		StepFunc{StepName: StepGenerate, Fn: func(context.Context) (StepLog, error) {
			t.Fatal("must stop after error")
			return StepLog{}, nil
		}},
	}}
	logs, err := p.Run(context.Background())
	if !errors.Is(err, boom) || len(logs) != 2 || len(seen) != 2 {
		t.Fatalf("logs=%v err=%v", logs, err)
	}
	if logs[0].Step != StepCropPlan || logs[0].JobID != "42" || logs[0].Ms < 1 || logs[0].Crops != 20 {
		t.Fatalf("log0 = %+v", logs[0])
	}
	if logs[1].Error != "boom" || !logs[1].Degraded {
		t.Fatalf("log1 = %+v", logs[1])
	}
	if StageOf(StepCropCheck) != StageCropping || StageOf(StepPrompt) != StagePrompting || StageOf(StepTransfer) != StageGenerating {
		t.Fatal("stages")
	}
}
