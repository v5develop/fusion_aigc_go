package svc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Kick 不阻塞调用方；跑的期间多次 Kick 合并成一轮补跑。
func TestAsyncSyncCoalesces(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	a := NewAsyncSync(func(context.Context) error {
		if calls.Add(1) == 1 {
			<-gate
		}
		return nil
	})
	start := time.Now()
	a.Kick()
	for i := 0; i < 5; i++ {
		a.Kick()
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Kick 不应阻塞")
	}
	close(gate)
	a.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("跑的期间的多次 Kick 应合并为一轮补跑: %d", n)
	}
}

// 暂时失败重试（最多 3 次），仍失败只记日志、不阻塞后续 Kick。
func TestAsyncSyncRetries(t *testing.T) {
	var calls atomic.Int32
	a := NewAsyncSync(func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("db blip")
		}
		return nil
	})
	a.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	a.Kick()
	a.Wait()
	if calls.Load() != 3 {
		t.Fatalf("应重试到成功: %d", calls.Load())
	}

	calls.Store(0)
	b := NewAsyncSync(func(context.Context) error { calls.Add(1); return errors.New("down") })
	b.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	b.Kick()
	b.Wait()
	if calls.Load() != 3 {
		t.Fatalf("最多尝试 3 次: %d", calls.Load())
	}
	b.Kick()
	b.Wait()
	if calls.Load() != 6 {
		t.Fatal("失败后再 Kick 应照常执行")
	}
}

func TestAsyncSyncNil(t *testing.T) {
	var a *AsyncSync
	a.Kick()
	a.Wait()
	NewAsyncSync(nil).Kick()
}
