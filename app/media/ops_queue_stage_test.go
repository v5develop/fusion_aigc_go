package media

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func queueStages(timings []StageTiming, stage string) []StageTiming {
	var out []StageTiming
	for _, t := range timings {
		if t.Stage == stage {
			out = append(out, t)
		}
	}
	return out
}

// 计算名额排队的等待时长记成 queue_compute 阶段（日志 + 响应 timings），只记等待不记执行。
func TestComputeSlotWaitRecordedAsQueueStage(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 5, TempDir: t.TempDir()})
	release, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	ctx, timings := withStageTimings(context.Background())
	err = svc.compute(ctx, func() error {
		time.Sleep(300 * time.Millisecond) // 执行时长不应计入排队阶段
		return nil
	})
	if err != nil {
		t.Fatalf("名额释放后应能执行: %v", err)
	}
	got := queueStages(timings.snapshot(), "queue_compute")
	if len(got) != 1 {
		t.Fatalf("应记录一条 queue_compute，得到 %+v", timings.snapshot())
	}
	if got[0].DurMs < 150 || got[0].DurMs >= 450 {
		t.Fatalf("queue_compute 应只含等待（约 200ms），得到 %dms", got[0].DurMs)
	}
	if n := len(queueStages(timings.snapshot(), "queue_io")); n != 0 {
		t.Fatalf("计算名额不应记 queue_io，得到 %d 条", n)
	}
}

// 输入名额的等待记成 queue_io；每次等待各记一条，按发生顺序排列。
func TestIOSlotWaitRecordedPerWait(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 5, TempDir: t.TempDir()})
	ctx, timings := withStageTimings(context.Background())
	if err := svc.withIO(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := svc.compute(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := svc.withIO(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, st := range timings.snapshot() {
		order = append(order, st.Stage)
	}
	want := []string{"queue_io", "queue_compute", "queue_io"}
	if len(order) != len(want) {
		t.Fatalf("阶段顺序应为 %v，得到 %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("阶段顺序应为 %v，得到 %v", want, order)
		}
	}
}

// 排队超时：阶段照样记录（时长约为排队上限），错误仍是 503 transient 可重试。
func TestComputeSlotTimeoutStillRecordsQueueStage(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 1, TempDir: t.TempDir()})
	release, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, timings := withStageTimings(context.Background())
	_, err = svc.acquire(ctx)
	if err == nil {
		t.Fatal("名额被占满时应超时失败")
	}
	ae := classifyError(err)
	if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
		t.Fatalf("应为 503 transient 且可重试，得到 %+v status=%d", ae, ae.status)
	}
	got := queueStages(timings.snapshot(), "queue_compute")
	if len(got) != 1 {
		t.Fatalf("超时也应记录一条 queue_compute，得到 %+v", timings.snapshot())
	}
	if got[0].DurMs < 900 || got[0].DurMs > 5000 {
		t.Fatalf("超时的 queue_compute 应约为排队上限 1s，得到 %dms", got[0].DurMs)
	}
}
