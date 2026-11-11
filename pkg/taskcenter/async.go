package taskcenter

import (
	"context"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// AsyncSync 把任务中心同步移出请求 / 收尾路径（与 canvas svc.AsyncSync 同一语义，供 studio 等其它服务复用）：
// Kick 立即返回；后台同一时刻只跑一轮，跑的期间再有 Kick 只记一个「脏」标记，本轮结束后补跑一轮（合并多次请求）。
// 每轮暂时失败最多再试 2 次（退避 0.5s / 1s），仍失败只记日志——各服务的后台扫描每轮也会窗口同步一次兜底。
type AsyncSync struct {
	name    string
	sync    func(ctx context.Context) error
	backoff []time.Duration
	timeout time.Duration

	mu      sync.Mutex
	running bool
	dirty   bool
	wg      sync.WaitGroup
}

// NewAsyncSync name 用于日志；fn 为 nil 时 Kick 是空操作。
func NewAsyncSync(name string, fn func(ctx context.Context) error) *AsyncSync {
	return &AsyncSync{name: name, sync: fn, backoff: []time.Duration{500 * time.Millisecond, time.Second}, timeout: 30 * time.Second}
}

// Kick 请求一轮同步（不阻塞）。
func (a *AsyncSync) Kick() {
	if a == nil || a.sync == nil {
		return
	}
	a.mu.Lock()
	if a.running {
		a.dirty = true
		a.mu.Unlock()
		return
	}
	a.running = true
	a.wg.Add(1)
	a.mu.Unlock()
	go a.loop()
}

// Wait 等后台这一轮（含补跑）结束（单测 / 优雅退出用）。
func (a *AsyncSync) Wait() {
	if a != nil {
		a.wg.Wait()
	}
}

func (a *AsyncSync) loop() {
	defer a.wg.Done()
	for {
		a.once()
		a.mu.Lock()
		if !a.dirty {
			a.running = false
			a.mu.Unlock()
			return
		}
		a.dirty = false
		a.mu.Unlock()
	}
}

func (a *AsyncSync) once() {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("%s: 任务中心异步同步 panic: %v", a.name, r)
		}
	}()
	var err error
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
		err = a.sync(ctx)
		cancel()
		if err == nil || i >= len(a.backoff) {
			break
		}
		time.Sleep(a.backoff[i])
	}
	if err != nil {
		logx.Errorf("%s: 任务中心异步同步失败（后台扫描下一轮兜底）: %v", a.name, err)
	}
}
