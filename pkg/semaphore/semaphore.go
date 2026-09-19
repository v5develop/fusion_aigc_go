// Package semaphore 按 key 限制并发数，用于遵守模型的 max_concurrency。
package semaphore

import (
	"context"
	"sync"
)

type Group struct {
	mu   sync.Mutex
	sems map[int64]chan struct{}
}

func NewGroup() *Group {
	return &Group{sems: map[int64]chan struct{}{}}
}

// Acquire 取一个名额，size <= 0 表示不限并发。
// 返回的 release 必须调用；ctx 结束前拿不到名额则返回 ctx.Err()。
func (g *Group) Acquire(ctx context.Context, key int64, size int) (release func(), err error) {
	if size <= 0 {
		return func() {}, nil
	}
	g.mu.Lock()
	sem, ok := g.sems[key]
	if !ok || cap(sem) != size { // 模型换了并发上限时重建
		sem = make(chan struct{}, size)
		g.sems[key] = sem
	}
	g.mu.Unlock()

	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
