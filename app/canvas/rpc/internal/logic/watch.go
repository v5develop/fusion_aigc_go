package logic

// 运行中 gateway run 的完成检测主路径：每个 run 一个长轮询协程。
//
//	RunNode 内联提交成功（或扫描重提成功 / 扫描接手没人管的 run）→ 放掉租约 → WatchRun(id)
//	  → 协程抢 run 租约（pkg/lease.Keep，后台心跳续租）→ 循环 GET /v1/calls/{id}?wait=LongPollWait，直到全部调用终态
//	  → 与扫描同一套收尾（apply：转存 → CAS 成功 → 结算 → 入素材库；失败 / 取消退款；UNKNOWN 待对账）；未终态但超时 → 置失败退款并取消调用。
//
// 扫描（Poller.Tick，Poll.IntervalSeconds）降为兜底：只列没有租约的 run，长轮询协程持有租约期间扫描看不到它；
// 名额满（Poll.Watchers）、进程重启、协程出错退出、租约被接管时，扫描照常 batchGet 收尾，并把仍在跑的 run 再交给长轮询协程。
// 进程停止（Poller.Stop）取消全部协程的 ctx：挂起的长轮询立即返回，协程放掉租约退出，其它实例按租约接管。
// 收尾的每一步都是 CAS / 幂等键（转存 canvas-copy:{task}:{i}、结算 Settle 幂等），与扫描或别的实例并发也不会重复扣费。

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/lease"

	"github.com/zeromicro/go-zero/core/logx"
)

// watchMinGap 长轮询循环相邻两圈的最小间隔：网关没按 wait 挂起（立即返回未终态）时不至于空转打满网关。变量便于单测调小。
var watchMinGap = time.Second

// keepFunc 抢 run 租约并心跳续租（默认 RunRepo.KeepLease；单测替换）。
type keepFunc func(ctx context.Context, id int64) (context.Context, func(), bool, error)

// watcher 长轮询协程池。
type watcher struct {
	c     *core
	ctx   context.Context // Poller 的根 ctx：Stop 时取消
	slots chan struct{}
	keep  keepFunc

	inflight sync.Map // run id → 本进程已有协程（本地去重；跨实例互斥靠行租约）
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
}

func newWatcher(ctx context.Context, c *core) *watcher {
	n := c.s.Config.Poll.Watchers
	if n <= 0 {
		n = 64
	}
	w := &watcher{c: c, ctx: ctx, slots: make(chan struct{}, n)}
	w.keep = func(ctx context.Context, id int64) (context.Context, func(), bool, error) {
		return c.s.Repo.Run.KeepLease(ctx, id, lease.Owner(), runLeaseTTL)
	}
	return w
}

// Watch 给 run 起长轮询协程（不阻塞）。已在本进程长轮询、名额已满、已关闭或网关未接入时返回 false（交给扫描兜底）。
func (w *watcher) Watch(id int64) bool {
	if w == nil || id <= 0 || !w.c.s.Gateway.Enabled() {
		return false
	}
	if _, loaded := w.inflight.LoadOrStore(id, struct{}{}); loaded {
		return false
	}
	select {
	case w.slots <- struct{}{}:
	default:
		w.inflight.Delete(id)
		return false
	}
	w.mu.Lock()
	if w.closed || w.ctx.Err() != nil {
		w.mu.Unlock()
		<-w.slots
		w.inflight.Delete(id)
		return false
	}
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		defer func() { <-w.slots }()
		defer w.inflight.Delete(id)
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("canvas: run %d 长轮询 panic: %v", id, r)
			}
		}()
		w.run(id)
	}()
	return true
}

// close 不再接新的协程，等在跑的全部退出（调用方先取消根 ctx）。
func (w *watcher) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.wg.Wait()
}

func (w *watcher) run(id int64) {
	lctx, stop, ok, err := w.keep(w.ctx, id)
	if err != nil {
		logx.Errorf("canvas: run %d 长轮询抢租约失败（交给扫描）: %v", id, err)
		return
	}
	if !ok {
		return // 别的实例 / 扫描正在处理
	}
	defer stop()
	w.c.watchRun(lctx, id)
}

// watchRun 长轮询一个运行中的 gateway run 直到收尾（调用方持有租约，ctx 随租约丢失 / 进程停止取消）。
// 出错（查库 / 查网关失败）就退出，交给扫描兜底，不在这里重试。
func (c *core) watchRun(ctx context.Context, id int64) {
	wait := c.s.Config.LongPollWait()
	done := map[string]*aigateway.Call{} // 已终态的调用（终态不再变，不必再查）
	for ctx.Err() == nil {
		start := time.Now()
		run, err := c.s.Repo.Run.Get(ctx, id)
		if err != nil || run == nil || run.Status != model.RunRunning || run.Backend != model.BackendGateway {
			return // 已收尾（取消 / 强制失败 / 别处完成）或不归这里管
		}
		ids := callIDs(run)
		if len(ids) == 0 {
			return
		}
		var pending []string
		for _, cid := range ids {
			if done[cid] == nil {
				pending = append(pending, cid)
			}
		}
		if len(pending) > 0 {
			call, err := c.s.Gateway.Get(ctx, pending[0], wait)
			if err != nil {
				if ctx.Err() == nil {
					logx.WithContext(ctx).Infof("canvas: run %d 长轮询 gateway 调用 %s 失败（交给扫描）: %v", run.ID, pending[0], err)
				}
				return
			}
			if call.IsTerminal() {
				done[pending[0]] = call
				// 这一个到终态了：其余的一次批量查完（count>1 时各张通常差不多同时出图），没到终态的下一圈接着长轮询
				if rest := pending[1:]; len(rest) > 0 {
					if got, err := c.s.Gateway.BatchGet(ctx, rest); err == nil {
						for j := range got {
							if got[j].IsTerminal() {
								done[got[j].ID] = &got[j]
							}
						}
					}
				}
			}
		}
		cs := make([]*aigateway.Call, len(ids))
		for i, cid := range ids {
			cs[i] = done[cid]
		}
		o := ClassifyGateway(run.Kind, cs)
		o.calls = cs
		if o.done {
			c.apply(ctx, run, o)
			return
		}
		if c.expired(run) {
			if c.failRun(ctx, run, model.RunRunning, "TIMEOUT", "生成超时，点数已退回", true) {
				c.cancelCalls(ctx, run.ID, ids, "生成超时")
			}
			return
		}
		if gap := watchMinGap - time.Since(start); gap > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(gap):
			}
		}
	}
}

// handOff run 刚转运行中（或扫描看到它仍在跑）：交给长轮询协程。调用方须已放掉该 run 的租约。
func (c *core) handOff(run *model.NodeRun) {
	if c.s.WatchRun != nil && run != nil && run.Backend == model.BackendGateway && run.Status == model.RunRunning {
		c.s.WatchRun(run.ID)
	}
}
