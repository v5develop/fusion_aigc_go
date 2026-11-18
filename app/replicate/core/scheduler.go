package core

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/replicate/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// ---------------------------------------------------------------------------------------------
// 后台推进的并发调度器（CONTRACT §AC「去掉自造排队」）。
//
// 原来一个 goroutine 每 2 秒串行跑：分析 → job → 入库（入库一次最长 12 分钟）→ 任务中心同步，一条慢的 media / 网关调用
// 卡住后面所有任务。现在分成三条互不相干的「车道」（分析 / job / 入库），每条车道是一个持续补位的有界 worker 集合：
//
//	取数协程：按 next_run_at 升序取一批到期行（上限 batch）→ 去掉本进程已在处理的 → 按空闲名额起 worker（不等它们做完）
//	worker ：抢行租约 → 推进 → 写回 next_run_at → 释放租约（ProcessAnalysis / ProcessJob；入库用发件箱自己的认领令牌）
//	补位   ：worker 做完立即唤醒取数协程（最短间隔 refillGap，合并多次唤醒），积压时不用等下一个 interval
//
// 不变量：
//   - 每条车道同时处理的行数 ≤ 该车道并发上限（运行时可调，调小时只是暂不补位，不打断在途的）；
//   - 同一行在本进程内同一时刻只有一个 worker（本地去重 + 行租约），多实例之间靠行租约（lease.Set，TTL/3 后台续租）互斥；
//   - 车道之间不共享名额：慢的分析 / 入库占满自己的车道，也不影响 job 推进；
//   - 公平：取数按 next_run_at 升序、名额不够时先到期的先起，推过的行写回新的 next_run_at 排到后面，不会饿死。
//
// 抢租约在 worker 里做（调度器记录每条已持有的行）：没抢到直接放弃（不计排队时长）；抢租约出错（库故障）本进程内对这行退避 errBackoffLocal，
// 不会每次取数都对同一行空转。租约派生的 ctx 在租约丢失时取消，worker 随之停手。
//
// 在途调用批量查状态：job worker 抢到租约后登记到合并窗口（callBatcher），窗口内的 job 合成一次 InFlightCalls + batchGet（≤200 一批），
// worker 推进到「查在途调用」时才等结果（有硬时限）；取数协程从不等它。
//
// 停机（Run 的 ctx 结束）：停止派发 → 等在途 worker 最多 StopGrace（默认 4 秒）→ 取消全部 worker → 在 ReleaseTimeout 内由调度器
// 统一释放记录在案的行租约与入库认领（不依赖 worker 是否返回）→ 再等 worker 最多 ReleaseTimeout 就返回。总耗时有硬上限，
// 须小于 go-zero 的 Shutdown.WaitTime（见 deploy/etc/gateway-api.yaml）。
// ---------------------------------------------------------------------------------------------

// 车道名（指标 kind 标签）。
const (
	KindAnalysis = "analysis"
	KindJob      = "job"
	KindDelivery = "delivery"
)

// 指标（go-zero metric → Prometheus，/metrics 由 go-zero DevServer 暴露）。
var (
	engineBacklog = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "backlog",
		Help:   "视频做同款后台推进：最近一次取数时到期但本进程还没开始处理的行数（上限 = 取数批量）",
		Labels: []string{"kind"},
	})
	engineBusy = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "busy",
		Help:   "视频做同款后台推进：正在处理的行数（worker 数）",
		Labels: []string{"kind"},
	})
	engineCapacity = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "capacity",
		Help:   "视频做同款后台推进：车道并发上限（busy / capacity = 饱和度）",
		Labels: []string{"kind"},
	})
	engineSaturated = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "saturated_total",
		Help:   "视频做同款后台推进：取数时名额已满、有到期行只能留到下一次的次数",
		Labels: []string{"kind"},
	})
	engineWait = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "wait_seconds",
		Help:    "视频做同款后台推进：到期（next_run_at）→ 开始处理的排队时长（从没排过的行不计）",
		Labels:  []string{"kind"},
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 30, 60, 120, 300, 900},
	})
	engineDuration = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "duration_seconds",
		Help:    "视频做同款后台推进：单行处理耗时（含抢租约、推进、写回）",
		Labels:  []string{"kind"},
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 720},
	})
	engineFetchErrors = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "fetch_errors_total",
		Help:   "视频做同款后台推进：取到期行失败的次数（失败时 backlog 置 0）",
		Labels: []string{"kind"},
	})
	engineLeaseErrors = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "lease_errors_total",
		Help:   "视频做同款后台推进：抢行租约出错（库故障）的次数，出错的行本进程内短暂退避",
		Labels: []string{"kind"},
	})
	engineBatchGet = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "replicate", Subsystem: "engine", Name: "batch_get_total",
		Help:   "视频做同款后台推进：在途调用批量查状态（网关 batchGet）次数，按结果",
		Labels: []string{"result"},
	})
)

// EngineSizes 三条车道的并发上限（≤0 用默认）。
type EngineSizes struct {
	Job, Analysis, Delivery int
}

// 默认并发（CONTRACT §AC）。
const (
	DefaultEngineConcurrency   = 16
	DefaultAnalysisConcurrency = 8
	DefaultDeliveryConcurrency = 8
)

func (z EngineSizes) withDefaults() EngineSizes {
	if z.Job <= 0 {
		z.Job = DefaultEngineConcurrency
	}
	if z.Analysis <= 0 {
		z.Analysis = DefaultAnalysisConcurrency
	}
	if z.Delivery <= 0 {
		z.Delivery = DefaultDeliveryConcurrency
	}
	return z
}

// Normalized 补齐默认值后的并发。
func (z EngineSizes) Normalized() EngineSizes { return z.withDefaults() }

// Total 三条车道合计（DB 连接池核算用）。
func (z EngineSizes) Total() int { z = z.withDefaults(); return z.Job + z.Analysis + z.Delivery }

// EngineConfig 调度器参数。
type EngineConfig struct {
	// Sizes 每次取数时读取的车道并发上限（运行时可调：afv_system_config 覆盖 yaml，并按 DB 连接池封顶）；nil = 全部默认。
	Sizes func() EngineSizes
	// Batch 每次取数的上限（0 = 该车道并发上限的 2 倍）。取得比名额多，多出来的只用于积压指标、留给下一次。
	Batch int
	// Interval 没有积压时的取数间隔（默认 2 秒）。
	Interval time.Duration
	// StopGrace 停机后等在途 worker 自然做完的时长（默认 4 秒），超过就取消它们并统一释放租约。
	StopGrace time.Duration
	// ReleaseTimeout 停机时统一释放租约 / 认领的硬上限，以及之后再等 worker 退出的上限（默认 1 秒）。
	ReleaseTimeout time.Duration
	// MergeWindow 在途调用批量查状态的合并窗口（默认 300ms）；PrefetchTimeout 每次批量查的时限（默认 2 秒）。
	MergeWindow     time.Duration
	PrefetchTimeout time.Duration
	// LeaseErrBackoff 抢租约出错的行在本进程内跳过多久（默认 5 秒）。
	LeaseErrBackoff time.Duration
}

// 默认值。
const (
	DefaultStopGrace      = 4 * time.Second
	DefaultReleaseTimeout = time.Second
)

// MaxStopDuration 停机最长耗时（StopGrace + 释放 + 等 worker 退出），用于核对 go-zero Shutdown.WaitTime。
func (c EngineConfig) MaxStopDuration() time.Duration {
	c = c.withDefaults()
	return c.StopGrace + 2*c.ReleaseTimeout
}

func (c EngineConfig) withDefaults() EngineConfig {
	if c.Interval < 50*time.Millisecond {
		c.Interval = 2 * time.Second
	}
	if c.StopGrace <= 0 {
		c.StopGrace = DefaultStopGrace
	}
	if c.ReleaseTimeout <= 0 {
		c.ReleaseTimeout = DefaultReleaseTimeout
	}
	if c.MergeWindow <= 0 {
		c.MergeWindow = 300 * time.Millisecond
	}
	if c.PrefetchTimeout <= 0 {
		c.PrefetchTimeout = 2 * time.Second
	}
	if c.LeaseErrBackoff <= 0 {
		c.LeaseErrBackoff = 5 * time.Second
	}
	if c.Sizes == nil {
		c.Sizes = func() EngineSizes { return EngineSizes{} }
	}
	return c
}

// refillGap 积压时两次取数的最短间隔：合并多个 worker 的完成唤醒，避免每做完一行就查一次库。
const refillGap = 200 * time.Millisecond

// ServiceSource 每次取数时拿当前内核（运行参数变化会重建 Service；nil = 暂不可用，本次跳过）。
type ServiceSource func(ctx context.Context) *Service

// Engine 三车道并发调度器。Run 阻塞到 ctx 结束，并在有上限的时间内收尾。
type Engine struct {
	src     ServiceSource
	guard   Guard
	cfg     EngineConfig
	batcher *callBatcher
	lanes   []*lane
}

// NewEngine 建调度器。guard 为 nil 时不抢行租约（只用于单实例测试）。
func NewEngine(src ServiceSource, guard Guard, cfg EngineConfig) *Engine {
	cfg = cfg.withDefaults()
	e := &Engine{src: src, guard: guard, cfg: cfg, batcher: newCallBatcher(cfg.MergeWindow, cfg.PrefetchTimeout)}
	sizes := func() EngineSizes { return cfg.Sizes().withDefaults() }
	e.lanes = []*lane{
		{kind: KindJob, table: model.TableJob, size: func() int { return sizes().Job },
			fetch: func(ctx context.Context, s *Service, n int) ([]DueItem, error) { return s.DueJobItems(ctx, n) },
			process: func(ctx context.Context, s *Service, id int64) {
				s.AdvanceJobHeld(withCallFuture(ctx, e.batcher.add(s, id)), id)
			}},
		{kind: KindAnalysis, table: model.TableAnalysis, size: func() int { return sizes().Analysis },
			fetch:   func(ctx context.Context, s *Service, n int) ([]DueItem, error) { return s.DueAnalysisItems(ctx, n) },
			process: func(ctx context.Context, s *Service, id int64) { s.AdvanceAnalysisHeld(ctx, id) }},
		{kind: KindDelivery, size: func() int { return sizes().Delivery },
			fetch:   func(ctx context.Context, s *Service, n int) ([]DueItem, error) { return s.DueDeliveryItems(ctx, n) },
			process: func(ctx context.Context, s *Service, id int64) { s.ProcessDelivery(ctx, id) }},
	}
	for _, l := range e.lanes {
		l.active = map[int64]*entry{}
		l.backoff = map[int64]time.Time{}
		l.kick = make(chan struct{}, 1)
	}
	return e
}

// Run 启动三条车道，阻塞到 ctx 结束；之后不再派发，按「宽限 → 取消 → 统一释放 → 有限等待」收尾，耗时 ≤ MaxStopDuration。
func (e *Engine) Run(ctx context.Context) {
	// worker 用独立的 ctx：停机时先给宽限期把手上这一行做完（写回 next_run_at、释放租约），而不是立刻全部中断
	wctx, wcancel := context.WithCancel(context.WithoutCancel(ctx))
	defer wcancel()
	var feeders sync.WaitGroup
	for _, l := range e.lanes {
		feeders.Add(1)
		go func(l *lane) {
			defer feeders.Done()
			l.feed(ctx, wctx, e)
		}(l)
	}
	<-ctx.Done()
	feeders.Wait()
	done := make(chan struct{})
	go func() {
		for _, l := range e.lanes {
			l.wg.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(e.cfg.StopGrace):
	}
	logx.Infof("replicate: 停机宽限 %s 已到，取消仍在处理的行并统一释放租约 / 认领", e.cfg.StopGrace)
	wcancel()
	rctx, rcancel := context.WithTimeout(context.Background(), e.cfg.ReleaseTimeout)
	e.releaseAll(rctx)
	rcancel()
	select {
	case <-done:
	case <-time.After(e.cfg.ReleaseTimeout):
		logx.Errorf("replicate: 判据=ENGINE_STOP_WORKER_STUCK 仍有 worker 没退出（不理会取消），不再等待；它们的租约 / 认领已释放")
	}
}

// releaseAll 统一释放所有车道记录在案、worker 还没自己释放的行租约与入库认领（并发执行，受 ctx 截止时间约束）。
func (e *Engine) releaseAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, l := range e.lanes {
		l.mu.Lock()
		for id, en := range l.active {
			if en.released {
				continue
			}
			switch {
			case l.table != "" && en.held && e.guard != nil:
				en.released = true
				wg.Add(1)
				go func(table string, id int64) { defer wg.Done(); e.guard.Release(ctx, table, id) }(l.table, id)
			case l.table == "" && en.token != "":
				en.released = true
				wg.Add(1)
				go func(svc *Service, id int64, token string) {
					defer wg.Done()
					if err := svc.ReleaseDeliveryClaim(ctx, id, token); err != nil {
						logx.Errorf("replicate: 停机释放入库认领失败 delivery=%d（等认领过期）: %v", id, err)
					}
				}(en.svc, id, en.token)
			}
		}
		l.mu.Unlock()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		logx.Errorf("replicate: 判据=ENGINE_STOP_RELEASE_TIMEOUT 停机释放租约超时，没放掉的等租约过期")
	}
}

// Busy 某车道正在处理的行数（测试 / 观测用）。
func (e *Engine) Busy(kind string) int {
	for _, l := range e.lanes {
		if l.kind == kind {
			l.mu.Lock()
			defer l.mu.Unlock()
			return len(l.active)
		}
	}
	return 0
}

// entry 一条正在处理的行：held = 已持有行租约（job / 分析）；token = 入库认领令牌；released = 已释放（worker 或停机统一释放，只放一次）。
type entry struct {
	svc      *Service
	held     bool
	token    string
	released bool
}

type lane struct {
	kind string
	// table 非空：worker 先经 Guard 抢这张表的行租约（job / 分析）；空：入库，用发件箱自己的认领令牌
	table   string
	size    func() int
	fetch   func(ctx context.Context, s *Service, limit int) ([]DueItem, error)
	process func(ctx context.Context, s *Service, id int64)

	mu      sync.Mutex
	active  map[int64]*entry
	backoff map[int64]time.Time // 抢租约出错的行：到点前本进程不再派发
	wg      sync.WaitGroup
	kick    chan struct{}
}

// feed 取数协程：持续补位，直到 ctx 结束。
func (l *lane) feed(ctx, wctx context.Context, e *Engine) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		more := l.round(ctx, wctx, e)
		var kick <-chan struct{}
		if more {
			kick = l.kick // 有积压：worker 做完就补位
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(e.cfg.Interval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-kick:
			select { // 合并短时间内的多次唤醒
			case <-ctx.Done():
				return
			case <-time.After(refillGap):
			}
		}
	}
}

// round 一次取数 + 派发；返回是否还有到期行留着没起（积压）。只做取数与起 goroutine，不做任何可能慢的调用。
func (l *lane) round(ctx, wctx context.Context, e *Engine) (more bool) {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("replicate: 调度取数 panic kind=%s（下一次继续）: %v", l.kind, r)
			more = false
		}
	}()
	size := max(l.size(), 1)
	engineCapacity.Set(float64(size), l.kind)
	svc := e.src(ctx) // 运行时总开关只拦新请求；在途任务照常推进、收尾、退点
	if svc == nil || ctx.Err() != nil {
		return false
	}
	batch := e.cfg.Batch
	if batch <= 0 {
		batch = size * 2
	}
	items, err := l.fetch(ctx, svc, batch)
	if err != nil {
		if ctx.Err() == nil {
			engineFetchErrors.Inc(l.kind)
			engineBacklog.Set(0, l.kind)
			logx.WithContext(ctx).Errorf("replicate: 取到期行失败 kind=%s: %v", l.kind, err)
		}
		return false
	}
	now := time.Now()
	l.mu.Lock()
	for id, until := range l.backoff {
		if !now.Before(until) {
			delete(l.backoff, id)
		}
	}
	free := size - len(l.active)
	var pending []DueItem
	for _, it := range items {
		if _, running := l.active[it.ID]; running {
			continue
		}
		if _, cooling := l.backoff[it.ID]; cooling {
			continue
		}
		pending = append(pending, it)
	}
	start := pending[:max(0, min(free, len(pending)))]
	for _, it := range start {
		l.active[it.ID] = &entry{svc: svc}
	}
	l.wg.Add(len(start))
	l.mu.Unlock()

	engineBacklog.Set(float64(len(pending)-len(start)), l.kind)
	if len(pending) > len(start) {
		engineSaturated.Inc(l.kind)
	}
	for _, it := range start {
		go l.work(wctx, e, svc, it)
	}
	// 积压：有到期行因名额不够没起；或者取满了一整批（库里可能还有更多）且这次全起了
	return len(pending) > len(start) || (len(items) >= batch && len(start) > 0)
}

// observeWait 已拿到这一行（抢到租约 / 认领成功）才计排队时长。
func (l *lane) observeWait(it DueItem) {
	if it.NextRunAt != nil {
		engineWait.ObserveFloat(max(time.Since(*it.NextRunAt).Seconds(), 0), l.kind)
	}
}

func (l *lane) work(ctx context.Context, e *Engine, svc *Service, it DueItem) {
	id := it.ID
	engineBusy.Inc(l.kind)
	t0 := time.Now()
	started := false
	defer func() {
		if r := recover(); r != nil {
			logx.WithContext(ctx).Errorf("replicate: 推进 panic kind=%s id=%d（下一次继续）: %v", l.kind, id, r)
		}
		if l.table != "" && e.guard != nil {
			l.mu.Lock()
			en := l.active[id]
			release := en != nil && en.held && !en.released
			if release {
				en.released = true
			}
			l.mu.Unlock()
			if release {
				e.guard.Release(ctx, l.table, id) // Set.Release 与调用方取消脱钩，停机后也能还回去
			}
		}
		if started {
			engineDuration.ObserveFloat(time.Since(t0).Seconds(), l.kind)
		}
		l.mu.Lock()
		delete(l.active, id)
		l.mu.Unlock()
		engineBusy.Dec(l.kind)
		l.wg.Done()
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}()
	if l.table == "" { // 入库：认领成功的回调里登记令牌、计排队时长
		ctx = withClaimHook(ctx, func(token string) {
			l.mu.Lock()
			if en := l.active[id]; en != nil {
				en.token = token
			}
			l.mu.Unlock()
			started = true
			l.observeWait(it)
		})
		l.process(ctx, svc, id)
		return
	}
	lctx := ctx
	if e.guard != nil {
		c, ok, err := e.guard.Acquire(ctx, l.table, id)
		if err != nil {
			engineLeaseErrors.Inc(l.kind)
			l.mu.Lock()
			l.backoff[id] = time.Now().Add(e.cfg.LeaseErrBackoff)
			l.mu.Unlock()
			logx.WithContext(ctx).Errorf("replicate: 抢租约出错，本进程 %s 内跳过这一行 判据=LEASE_ACQUIRE_ERROR kind=%s id=%d: %v", e.cfg.LeaseErrBackoff, l.kind, id, err)
			return
		}
		if !ok {
			return // 别的执行者正在推进
		}
		l.mu.Lock()
		if en := l.active[id]; en != nil {
			en.held = true
		}
		l.mu.Unlock()
		lctx = c
	}
	started = true
	l.observeWait(it)
	l.process(lctx, svc, id)
}
