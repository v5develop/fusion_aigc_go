package semaphore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireLimitsConcurrency(t *testing.T) {
	g := NewGroup()
	var cur, max int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Acquire(context.Background(), 9, 5)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if n := atomic.AddInt32(&cur, 1); n > atomic.LoadInt32(&max) {
				atomic.StoreInt32(&max, n)
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
		}()
	}
	wg.Wait()
	if max > 5 {
		t.Fatalf("并发数 %d 超过上限 5", max)
	}
	if max < 2 {
		t.Fatalf("并发数 %d 太低，信号量可能没放行", max)
	}
}

func TestAcquireUnlimited(t *testing.T) {
	g := NewGroup()
	release, err := g.Acquire(context.Background(), 1, 0)
	if err != nil || release == nil {
		t.Fatalf("size<=0 应不限并发: %v", err)
	}
	release()
}

func TestAcquireRespectsContext(t *testing.T) {
	g := NewGroup()
	release, err := g.Acquire(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, 1, 1); err == nil {
		t.Fatal("名额占满时应随 ctx 超时返回错误")
	}
}
