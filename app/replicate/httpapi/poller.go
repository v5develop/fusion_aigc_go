package httpapi

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/lease"

	"github.com/zeromicro/go-zero/core/logx"
)

// leaseGuard core.Guard 的生产实现：每张表一个 pkg/lease.Set（同 owner，后台每 TTL/3 统一续租），
// 抢不到（别的实例在推进 / 租约未过期）就跳过这一行。
type leaseGuard struct {
	sets map[string]*lease.Set
}

func newLeaseGuard(db lease.Execer, ttl time.Duration) *leaseGuard {
	owner := lease.Owner()
	return &leaseGuard{sets: map[string]*lease.Set{
		model.TableAnalysis: lease.NewSet(db, model.TableAnalysis, owner, ttl),
		model.TableJob:      lease.NewSet(db, model.TableJob, owner, ttl),
	}}
}

func (g *leaseGuard) Acquire(ctx context.Context, table string, id int64) bool {
	s := g.sets[table]
	if s == nil {
		return false
	}
	ok, err := s.Acquire(ctx, id)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 抢租约失败 表=%s id=%d: %v", table, id, err)
	}
	return ok
}

func (g *leaseGuard) Release(ctx context.Context, table string, id int64) {
	if s := g.sets[table]; s != nil {
		s.Release(ctx, id)
	}
}

func (g *leaseGuard) close() {
	for _, s := range g.sets {
		s.Close()
	}
}

// Poller 后台推进分析与出片任务（go-zero service.Service 形状：Start 阻塞到 Stop）。
// 多实例都可以跑：每一行推进前抢行租约，同一行同一时刻只由一个实例推进；接管后的幂等由内核的 CAS 与网关幂等键保证。
type Poller struct {
	rt       *Runtime
	guard    *leaseGuard
	interval time.Duration
	batch    int
	// taskSync 任务中心同步（每 taskSyncEvery 一轮；nil = 不同步）
	taskSync func(ctx context.Context) error
	lastSync time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	start  sync.Once
}

func newPoller(rt *Runtime, guard *leaseGuard, interval time.Duration, batch int) *Poller {
	if interval < 200*time.Millisecond {
		interval = 2 * time.Second
	}
	if batch <= 0 {
		batch = 20
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Poller{rt: rt, guard: guard, interval: interval, batch: batch, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Start 阻塞运行直到 Stop。
func (p *Poller) Start() {
	p.start.Do(func() {
		defer close(p.done)
		logx.Infof("replicate: 轮询器已启动（每 %s，每轮 ≤%d 条，行租约 owner=%s）", p.interval, p.batch, lease.Owner())
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			p.tick()
			select {
			case <-p.ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

func (p *Poller) tick() {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("replicate: 轮询一轮 panic（下一轮继续）: %v", r)
		}
	}()
	svc, _ := p.rt.Service(p.ctx) // 运行时总开关只拦新请求；在途任务照常推进、收尾、退点
	if svc == nil {
		return
	}
	svc.TickGuarded(p.ctx, p.batch, p.guard)
	svc.TickDeliveries(p.ctx, p.batch) // 入素材库（发件箱各自 CAS 认领，多实例安全）
	if p.taskSync != nil && time.Since(p.lastSync) >= taskSyncEvery {
		p.lastSync = time.Now()
		_ = p.taskSync(p.ctx) // 失败已在 taskcenter 里记日志，下一轮重来
	}
}

// taskSyncEvery 任务中心周期同步间隔（写接口另有 AsyncSync.Kick 即时触发）。
const taskSyncEvery = 5 * time.Second

// Stop 停止并等待退出（释放仍持有的租约）。
func (p *Poller) Stop() {
	p.once.Do(func() {
		p.cancel()
		started := true
		p.start.Do(func() { started = false; close(p.done) })
		if started {
			<-p.done
		}
		if p.guard != nil {
			p.guard.close()
		}
	})
}
