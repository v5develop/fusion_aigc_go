package logic

import (
	"sync"
	"sync/atomic"
)

// fanout 并发执行 fn(0)…fn(n-1)，同时最多 limit 个（limit<=1 或 n<=1 时按顺序串行），全部返回后才返回。
// 返回每个下标的错误（与下标一一对应）。fn 之间不得共享未加锁的可变状态：结果写进各自下标的槽位。
func fanout(n, limit int, fn func(i int) error) []error {
	errs := make([]error, n)
	if n <= 1 || limit <= 1 {
		for i := 0; i < n; i++ {
			errs[i] = fn(i)
		}
		return errs
	}
	sem := make(chan struct{}, min(limit, n))
	var (
		wg       sync.WaitGroup
		panicked atomic.Pointer[any]
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					panicked.CompareAndSwap(nil, &r)
				}
			}()
			errs[i] = fn(i)
		}()
	}
	wg.Wait()
	// fn 在子协程里 panic 不能直接打崩进程：等全部返回后在调用方协程重新抛出，与原先串行执行时 panic 的传播一致
	// （由 gRPC / Poller 的 recover 接住）
	if p := panicked.Load(); p != nil {
		panic(*p)
	}
	return errs
}

// firstErr 按下标顺序的第一个非 nil 错误（与原先逐个执行、遇错即停时返回的是同一个下标的错误）。
func firstErr(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// fanoutLimit 单个 run 内扇出的并发度（Poll.FanoutConcurrency，<=0 按 4）。
func (c *core) fanoutLimit() int {
	if n := c.s.Config.Poll.FanoutConcurrency; n > 0 {
		return n
	}
	return 4
}

// lookupLimit 扫描里 Java / studio 查询的并发度（Poll.LookupConcurrency，<=0 按 8）。
func (c *core) lookupLimit() int {
	if n := c.s.Config.Poll.LookupConcurrency; n > 0 {
		return n
	}
	return 8
}
