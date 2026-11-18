package httpapi

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/lease"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
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

func (g *leaseGuard) Acquire(ctx context.Context, table string, id int64) (context.Context, bool, error) {
	s := g.sets[table]
	if s == nil {
		return nil, false, nil
	}
	lctx, ok, err := s.AcquireCtx(ctx, id) // 租约丢失时 lctx 取消（心跳发现被别人持有 / 长时间续不上）
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 抢租约失败 表=%s id=%d: %v", table, id, err)
	}
	return lctx, ok, err
}

func (g *leaseGuard) Release(ctx context.Context, table string, id int64) {
	if s := g.sets[table]; s != nil {
		s.Release(ctx, id)
	}
}

// close 停止心跳并释放仍持有的租约，受 ctx 截止时间约束（停机硬上限）。
func (g *leaseGuard) close(ctx context.Context) {
	for _, s := range g.sets {
		s.CloseCtx(ctx)
	}
}

// Poller 后台推进（go-zero service.Service 形状：Start 阻塞到 Stop）。
// 推进交给 core.Engine：job / 分析 / 入素材库三条车道各自有界并发、持续补位，互不阻塞（CONTRACT §AC）；
// 多实例都可以跑：每一行推进前抢行租约，同一行同一时刻只由一个执行者推进；接管后的幂等由内核的 CAS 与网关幂等键保证。
// 灵感推荐预计算与任务中心同步在独立的协程里按自己的节奏跑，不占车道、也不被车道拖慢。
type Poller struct {
	rt       *Runtime
	guard    *leaseGuard
	engine   *core.Engine
	cfg      core.EngineConfig
	interval time.Duration
	// taskSync 任务中心同步（每 taskSyncEvery 一轮；nil = 不同步）
	taskSync func(ctx context.Context) error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	start  sync.Once
}

func newPoller(rt *Runtime, guard *leaseGuard, interval time.Duration, batch int, stopGrace time.Duration) *Poller {
	if interval < 200*time.Millisecond {
		interval = 2 * time.Second
	}
	if batch < 0 {
		batch = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Poller{rt: rt, guard: guard, interval: interval, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	var g core.Guard
	if guard != nil { // 别把 nil 指针装进接口
		g = guard
	}
	p.engine = core.NewEngine(func(ctx context.Context) *core.Service {
		svc, _ := rt.Service(ctx) // 运行时总开关只拦新请求；在途任务照常推进、收尾、退点
		return svc
	}, g, p.engineCfg(batch, stopGrace))
	return p
}

func (p *Poller) engineCfg(batch int, stopGrace time.Duration) core.EngineConfig {
	p.cfg = core.EngineConfig{Sizes: p.rt.EngineSizes, Batch: batch, Interval: p.interval, StopGrace: stopGrace}
	return p.cfg
}

// Start 阻塞运行直到 Stop。收到 SIGTERM 时（go-zero wrap-up 阶段，信号一到就触发）立即开始停机，不等 HTTP 先关。
func (p *Poller) Start() {
	p.start.Do(func() {
		defer close(p.done)
		proc.AddWrapUpListener(p.Stop)
		if max := p.cfg.MaxStopDuration() + closeTimeout; max >= 5500*time.Millisecond {
			logx.Infof("replicate: 停机最长 %s，须小于 gateway-api 的 Shutdown.WaitTime（go-zero 默认 5.5s 后强杀；deploy/etc/gateway-api.yaml 已配 10s）", max)
		}
		z := p.rt.EngineSizes()
		logx.Infof("replicate: 后台调度器已启动（无积压时每 %s 取数；并发 job=%d 分析=%d 入库=%d（0=默认 %d/%d/%d），行租约 owner=%s）",
			p.interval, z.Job, z.Analysis, z.Delivery, core.DefaultEngineConcurrency, core.DefaultAnalysisConcurrency, core.DefaultDeliveryConcurrency, lease.Owner())
		var side sync.WaitGroup
		side.Add(1)
		go func() {
			defer side.Done()
			p.sideLoop()
		}()
		p.engine.Run(p.ctx) // 阻塞到 Stop；返回前等在途 worker 收尾（最多宽限期）
		side.Wait()
	})
}

// sideLoop 灵感推荐预计算（每 interval 一次廉价的过期检查）与任务中心周期同步（每 taskSyncEvery），各自节奏，不占调度车道。
func (p *Poller) sideLoop() {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	var lastSync time.Time
	for {
		p.side(&lastSync)
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Poller) side(lastSync *time.Time) {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("replicate: 后台周期任务 panic（下一轮继续）: %v", r)
		}
	}()
	if svc, _ := p.rt.Service(p.ctx); svc != nil {
		svc.TickTrending(p.ctx)
	}
	if p.taskSync != nil && time.Since(*lastSync) >= taskSyncEvery {
		*lastSync = time.Now()
		_ = p.taskSync(p.ctx) // 失败已在 taskcenter 里记日志，下一轮重来
	}
}

// closeTimeout 停机兜底释放租约的硬上限。
const closeTimeout = 500 * time.Millisecond

// taskSyncEvery 任务中心周期同步间隔（写接口另有 AsyncSync.Kick 即时触发）。
const taskSyncEvery = 5 * time.Second

// Stop 停止派发并等待在途 worker 收尾（最多宽限期），再释放仍持有的租约。
func (p *Poller) Stop() {
	p.once.Do(func() {
		p.cancel()
		started := true
		p.start.Do(func() { started = false; close(p.done) })
		if started {
			<-p.done
		}
		if p.guard != nil { // 调度器已统一释放过；这里兜底还回剩下的（硬上限 closeTimeout）
			ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			p.guard.close(ctx)
			cancel()
		}
	})
}
