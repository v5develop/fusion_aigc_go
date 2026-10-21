package media

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 查询注册中心期间不能持有 owner 选择的锁：其它请求应拿缓存结果立即返回，而不是排队等 I/O。
func TestOwnerQueryDoesNotBlockOtherRequests(t *testing.T) {
	var clock struct {
		sync.Mutex
		t time.Time
	}
	clock.t = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clock.Lock(); defer clock.Unlock(); return clock.t }

	var slow atomic.Bool
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	p := &ownerPicker{self: "tst001", now: now}
	p.list = func(context.Context) ([]string, error) {
		if slow.Load() {
			entered <- struct{}{}
			<-gate // 模拟慢的注册中心查询
		}
		return []string{"peer01"}, nil
	}
	p.candidates(context.Background()) // 第一次成功查询，建立缓存

	clock.Lock()
	clock.t = clock.t.Add(ownerCacheTTL + time.Second) // 缓存过期，下一次必须重查
	clock.Unlock()

	slow.Store(true)
	done := make(chan struct{})
	go func() { p.candidates(context.Background()); close(done) }()
	<-entered // 第一个请求已经在锁外查询中

	other := make(chan []string, 1)
	go func() { other <- p.candidates(context.Background()) }()
	select {
	case ids := <-other:
		if len(ids) == 0 {
			t.Fatal("查询期间的请求应拿到缓存结果")
		}
	case <-time.After(2 * time.Second):
		close(gate)
		t.Fatal("查询期间其它请求被阻塞：查询不能在锁内进行")
	}
	close(gate)
	<-done
}

// 注册中心查询失败后进入退避：退避期间不重复查询；退避过后再查。
func TestOwnerQueryFailureBacksOff(t *testing.T) {
	var clock struct {
		sync.Mutex
		t time.Time
	}
	clock.t = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clock.Lock(); defer clock.Unlock(); return clock.t }

	var calls atomic.Int32
	p := &ownerPicker{self: "tst001", now: now}
	p.list = func(context.Context) ([]string, error) {
		calls.Add(1)
		return nil, errors.New("注册中心不可用")
	}

	for i := 0; i < 50; i++ {
		ids := p.candidates(context.Background())
		if len(ids) != 1 || ids[0] != "tst001" {
			t.Fatalf("从未成功时只能用本实例，得到 %v", ids)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("退避期间查询了 %d 次，应只查 1 次", n)
	}

	clock.Lock()
	clock.t = clock.t.Add(ownerRetryBackoff + time.Second)
	clock.Unlock()
	p.candidates(context.Background())
	if n := calls.Load(); n != 2 {
		t.Fatalf("退避过后应重新查询，共 %d 次", n)
	}
}
