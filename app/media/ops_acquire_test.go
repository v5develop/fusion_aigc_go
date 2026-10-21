package media

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// 并发名额被占满时，排队的请求在 min(OpTimeoutSeconds, 120s) 后返回 503 transient，而不是无限等待。
func TestAcquireQueueIsBoundedByOpTimeout(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 1, TempDir: t.TempDir()})
	release, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	start := time.Now()
	_, err = svc.acquire(context.Background()) // 没有 deadline_ms 的请求
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("名额被占满时第二个请求应失败")
	}
	if elapsed < 900*time.Millisecond || elapsed > 10*time.Second {
		t.Fatalf("排队时长 %v 不在 OpTimeoutSeconds(1s) 附近", elapsed)
	}
	ae := classifyError(err)
	if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
		t.Fatalf("应为 503 transient 且可重试，得到 %+v status=%d", ae, ae.status)
	}
}

// 名额释放后，排队中的请求能拿到名额（上限不影响正常排队）。
func TestAcquireGetsSlotWhenReleasedWithinWait(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 5, TempDir: t.TempDir()})
	release, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	release2, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatalf("名额释放后应能获得: %v", err)
	}
	release2()
}
