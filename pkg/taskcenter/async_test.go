package taskcenter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Kick 不阻塞；运行期间的多次 Kick 合并成一轮补跑；暂时失败按退避重试。
func TestAsyncSyncCoalescesAndRetries(t *testing.T) {
	var n atomic.Int32
	release := make(chan struct{})
	a := NewAsyncSync("t", func(ctx context.Context) error {
		if n.Add(1) == 1 {
			<-release
			return errors.New("busy")
		}
		return nil
	})
	a.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	start := time.Now()
	a.Kick()
	a.Kick()
	a.Kick()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Kick 不应阻塞")
	}
	close(release)
	a.Wait()
	// 第 1 次失败 → 重试成功（2 次）+ 合并的补跑 1 次 = 3
	if got := n.Load(); got != 3 {
		t.Fatalf("应重试一次并合并补跑一轮: %d", got)
	}
	var nilA *AsyncSync
	nilA.Kick()
	NewAsyncSync("t", nil).Kick()
}
