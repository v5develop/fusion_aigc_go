package core

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
)

// ---- 并发调度器（scheduler.go）用例：替身都是并发安全的，跑 -race ----

// memGuard 内存行租约：同一 (表, id) 同一时刻只有一个持有者；发现释放没持有的租约就记违规。
// steal 模拟租约被别人持有（取消持有者的 ctx，之后持有者的 Release 是空操作）；acquireErr 模拟库故障。
type memGuard struct {
	mu         sync.Mutex
	held       map[string]*memLease
	violations atomic.Int64
	acquired   atomic.Int64
	attempts   atomic.Int64
	acquireErr error
}

type memLease struct {
	cancel context.CancelFunc
	stolen bool
}

func newMemGuard() *memGuard { return &memGuard{held: map[string]*memLease{}} }

func (g *memGuard) key(table string, id int64) string { return fmt.Sprintf("%s/%d", table, id) }

func (g *memGuard) Acquire(ctx context.Context, table string, id int64) (context.Context, bool, error) {
	g.attempts.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.acquireErr != nil {
		return nil, false, g.acquireErr
	}
	if g.held[g.key(table, id)] != nil {
		return nil, false, nil
	}
	lctx, cancel := context.WithCancel(ctx)
	g.held[g.key(table, id)] = &memLease{cancel: cancel}
	g.acquired.Add(1)
	return lctx, true, nil
}

func (g *memGuard) Release(_ context.Context, table string, id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.held[g.key(table, id)]
	switch {
	case h == nil:
		g.violations.Add(1) // 释放了没持有的租约
	case h.stolen:
		// 已被别人持有：持有者的释放是空操作
	default:
		h.cancel()
		delete(g.held, g.key(table, id))
	}
}

func (g *memGuard) steal(table string, id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h := g.held[g.key(table, id)]; h != nil {
		h.stolen = true
		h.cancel()
	}
}

func (g *memGuard) holds(table string, id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.held[g.key(table, id)]
	return h != nil && !h.stolen
}

func (g *memGuard) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.held)
}

// concGW 包一层替身网关：统计并发、可选的提交延迟、批量读计数，并核对「动这个 job 的网关调用时一定持有它的租约」。
type concGW struct {
	*fakeGW
	guard       *memGuard
	submitDelay time.Duration
	block       chan struct{} // 非 nil：提交卡住且不理会 ctx（模拟卡死的 worker），关闭才放行
	entered     atomic.Int64
	cur, peak   atomic.Int64
	gets        atomic.Int64 // 单个 Get（出片步骤）
	batchCalls  atomic.Int64
	batchIDs    atomic.Int64
	noLease     atomic.Int64
}

func (g *concGW) checkLease(labels map[string]string) {
	if g.guard == nil || labels["job"] == "" {
		return
	}
	var id int64
	_, _ = fmt.Sscan(labels["job"], &id)
	if !g.guard.holds(model.TableJob, id) {
		g.noLease.Add(1)
	}
}

func (g *concGW) Submit(ctx context.Context, req aigateway.CallRequest, opt aigateway.SubmitOptions) (*aigateway.Call, error) {
	g.checkLease(req.Labels)
	n := g.cur.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer g.cur.Add(-1)
	g.entered.Add(1)
	if g.block != nil {
		<-g.block
	}
	if g.submitDelay > 0 {
		select {
		case <-time.After(g.submitDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.fakeGW.Submit(ctx, req, opt)
}

func (g *concGW) Get(ctx context.Context, id string, wait time.Duration) (*aigateway.Call, error) {
	g.mu.Lock()
	c := g.byID[id]
	g.mu.Unlock()
	if c != nil && c.Operation == "video.generate" {
		g.gets.Add(1)
	}
	return g.fakeGW.Get(ctx, id, wait)
}

func (g *concGW) BatchGet(ctx context.Context, ids []string) ([]aigateway.Call, error) {
	g.batchCalls.Add(1)
	g.batchIDs.Add(int64(len(ids)))
	out := make([]aigateway.Call, 0, len(ids))
	for _, id := range ids {
		c, err := g.fakeGW.Get(ctx, id, 0) // 替身：读一次即推进到终态（与单个 Get 同语义）
		if err == nil {
			out = append(out, *c)
		}
	}
	return out, nil
}

// lockedMedia 把替身媒体的调用串行化（替身本身不是并发安全的）；IngestURL 可被 gate 卡住（模拟慢的落桶）。
type lockedMedia struct {
	mu    sync.Mutex
	m     *fakeMedia
	gate  chan struct{}
	inIng atomic.Int64
}

func (l *lockedMedia) IngestURL(ctx context.Context, src, purpose, idem string) (mediasvc.Resource, error) {
	if l.gate != nil {
		l.inIng.Add(1)
		select {
		case <-l.gate:
		case <-ctx.Done():
			return mediasvc.Resource{}, ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.IngestURL(ctx, src, purpose, idem)
}

func (l *lockedMedia) Probe(ctx context.Context, rid string) (mediasvc.ProbeInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Probe(ctx, rid)
}

func (l *lockedMedia) InspectVideo(ctx context.Context, rid string) (mediasvc.VideoInspect, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.InspectVideo(ctx, rid)
}

func (l *lockedMedia) Frames(ctx context.Context, rid string, n int, idem string) ([]mediasvc.Frame, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Frames(ctx, rid, n, idem)
}

func (l *lockedMedia) Grid(ctx context.Context, rids []string, p mediasvc.GridParams, idem string) (mediasvc.Resource, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Grid(ctx, rids, p, idem)
}

// slowLib 复制到素材库卡在 gate 上（模拟慢的入库）；ignoreCtx 时连取消都不理（卡死）。
type slowLib struct {
	gate      chan struct{}
	entered   atomic.Int64
	ignoreCtx bool
}

func (s *slowLib) CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error) {
	s.entered.Add(1)
	if s.ignoreCtx {
		<-s.gate
		return mediasvc.Resource{}, ctx.Err()
	}
	select {
	case <-s.gate:
	case <-ctx.Done():
		return mediasvc.Resource{}, ctx.Err()
	}
	return mediasvc.Resource{RID: "lib-" + rid, URL: "https://pub.test/" + rid}, nil
}

// inflightStore 给内存 Store 补上「列在途调用号」（repo 的 InFlightCalls 同口径）。
type inflightStore struct{ *memStore }

func (s inflightStore) InFlightCalls(_ context.Context, jobIDs []int64) (map[int64][]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[int64]bool{}
	for _, id := range jobIDs {
		want[id] = true
	}
	out := map[int64][]string{}
	for _, st := range s.steps {
		if want[st.JobID] && st.Status == model.StepRunning && st.GatewayCallID != nil && *st.GatewayCallID != "" {
			out[st.JobID] = append(out[st.JobID], *st.GatewayCallID)
		}
	}
	return out, nil
}

// concEnv 真实时钟的场景（调度器按 next_run_at 取数，时钟要走），在途重推间隔压到 20ms。
type concEnv struct {
	*env
	cgw   *concGW
	media *lockedMedia
	lib   *slowLib
	ops   *memOps
	guard *memGuard
}

func newConcEnv(t *testing.T, withDelivery bool) *concEnv {
	e := &env{t: t, clock: time.Now(), ledger: newLedger(), gw: newGW(), media: &fakeMedia{}, ctx: context.Background()}
	e.store = newMemStore(time.Now)
	e.signer = &fakeSigner{}
	ce := &concEnv{env: e, guard: newMemGuard(), media: &lockedMedia{m: e.media}}
	ce.cgw = &concGW{fakeGW: e.gw, guard: ce.guard}
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	cfg.ActiveRepoll = 20 * time.Millisecond
	cfg.BadOutputOff = true
	opts := []Option{WithGateway(ce.cgw), WithMedia(ce.media), WithClock(time.Now), WithOwner("test-owner"),
		WithURLOwner(testOwner), WithSigner(e.signer)}
	if withDelivery {
		ce.ops = newMemOps(time.Now)
		ce.lib = &slowLib{gate: make(chan struct{})}
		opts = append(opts, WithDelivery(ce.ops, ce.lib, &fakeAssets{}, DeliveryConfig{MaxAttempts: 3}))
	}
	e.svc = New(cfg, inflightStore{e.store}, e.ledger, opts...)
	return ce
}

// start 跑调度器，返回停止函数（停止后等它完全退出）。
func (ce *concEnv) start(sizes EngineSizes) (*Engine, func()) {
	return ce.startCfg(EngineConfig{Sizes: func() EngineSizes { return sizes }, Interval: 50 * time.Millisecond, StopGrace: 2 * time.Second})
}

func (ce *concEnv) startCfg(cfg EngineConfig) (*Engine, func()) {
	eng := NewEngine(func(context.Context) *Service { return ce.svc }, ce.guard, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); eng.Run(ctx) }()
	return eng, func() { cancel(); <-done }
}

func (ce *concEnv) newJob(i int) int64 {
	ce.t.Helper()
	a := ce.seedAnalysis(10 + float64(i)/10)
	return ce.confirm(ce.plan(a), "", fmt.Sprintf("k%d", i)).JobID
}

func (ce *concEnv) jobStatus(id int64) int8 {
	j, _ := ce.store.GetJob(ce.ctx, id)
	if j == nil {
		return -1
	}
	return j.Status
}

// assertMoneyAll 多个 job 合计：已扣除 = 全部成功段之和（每段只扣一次），且没有留下冻结中的单。
func (ce *concEnv) assertMoneyAll(ids []int64) {
	ce.t.Helper()
	var want int64
	ce.ledger.mu.Lock()
	for _, id := range ids {
		for _, st := range ce.steps(id) {
			if st.Kind == model.StepKindGateway && st.Status == model.StepSucceeded && st.BillingRequestID != nil {
				if r := ce.ledger.res[*st.BillingRequestID]; r != nil {
					want += r.points
				}
			}
		}
	}
	ce.ledger.mu.Unlock()
	if got := ce.ledger.sum("CONFIRMED"); got != want || want == 0 {
		ce.t.Fatalf("已扣除 %d，应为成功段之和 %d", got, want)
	}
	if held := ce.ledger.sum("HELD"); held != 0 {
		ce.t.Fatalf("终态后仍有冻结 %d", held)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// 慢的分析（落桶卡住）占着分析车道，job 照常推进到成功。
func TestEngineSlowAnalysisDoesNotDelayJobs(t *testing.T) {
	ce := newConcEnv(t, false)
	ce.media.gate = make(chan struct{})
	if _, err := ce.svc.CreateAnalysis(ce.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://own.test/aigc/video/7/20261113/v.mp4?sign=abc"}); err != nil {
		t.Fatal(err)
	}
	jobID := ce.newJob(1)
	eng, stop := ce.start(EngineSizes{})
	defer stop()
	defer close(ce.media.gate)
	waitFor(t, 3*time.Second, "分析进入落桶", func() bool { return ce.media.inIng.Load() >= 1 })
	waitFor(t, 5*time.Second, "job 在分析卡住期间成功", func() bool { return ce.jobStatus(jobID) == model.JobSucceeded })
	if eng.Busy(KindAnalysis) != 1 {
		t.Fatalf("分析应仍在处理：busy=%d", eng.Busy(KindAnalysis))
	}
	if ce.guard.violations.Load() != 0 || ce.cgw.noLease.Load() != 0 {
		t.Fatalf("租约违规=%d 无租约调用=%d", ce.guard.violations.Load(), ce.cgw.noLease.Load())
	}
}

// 慢的入库（复制到公有桶卡住）占着入库车道，后来的 job 照常推进到成功。
func TestEngineSlowDeliveryDoesNotDelayJobs(t *testing.T) {
	ce := newConcEnv(t, true)
	first := ce.newJob(1)
	eng, stop := ce.start(EngineSizes{})
	defer stop()
	defer close(ce.lib.gate)
	waitFor(t, 5*time.Second, "第一条成功并开始入库", func() bool { return ce.jobStatus(first) == model.JobSucceeded && ce.lib.entered.Load() >= 1 })
	second := ce.newJob(2)
	waitFor(t, 5*time.Second, "入库卡住期间第二条成功", func() bool { return ce.jobStatus(second) == model.JobSucceeded })
	if eng.Busy(KindDelivery) < 1 {
		t.Fatalf("入库应仍在处理：busy=%d", eng.Busy(KindDelivery))
	}
}

// N 个 job 并行推进：同时在提交的数量 > 1 且不超过车道并发上限；全部成功；租约从不重复持有、网关调用都在持有租约时发生。
func TestEngineJobsAdvanceInParallelWithinLimit(t *testing.T) {
	ce := newConcEnv(t, false)
	ce.cgw.submitDelay = 150 * time.Millisecond
	const n = 8
	var ids []int64
	for i := 0; i < n; i++ {
		ids = append(ids, ce.newJob(i))
	}
	_, stop := ce.start(EngineSizes{Job: 4})
	t0 := time.Now()
	waitFor(t, 10*time.Second, "全部成功", func() bool {
		for _, id := range ids {
			if ce.jobStatus(id) != model.JobSucceeded {
				return false
			}
		}
		return true
	})
	elapsed := time.Since(t0)
	stop()
	if left := ce.guard.count(); left != 0 {
		t.Fatalf("处理完应释放全部租约：还持有 %d 条", left)
	}
	if p := ce.cgw.peak.Load(); p < 2 || p > 4 {
		t.Fatalf("并发提交峰值应在 2~4（车道上限 4）：%d", p)
	}
	if elapsed >= n*150*time.Millisecond {
		t.Fatalf("没有并行：%d 条用了 %s（串行至少 %s）", n, elapsed, n*150*time.Millisecond)
	}
	if ce.guard.violations.Load() != 0 || ce.cgw.noLease.Load() != 0 {
		t.Fatalf("租约违规=%d 无租约调用=%d", ce.guard.violations.Load(), ce.cgw.noLease.Load())
	}
	ce.assertMoneyAll(ids)
}

// 两个「实例」（两套调度器）共用租约与库：同一 job 同一时刻只有一个执行者，结果与单实例一致（钱不重复扣）。
func TestEngineTwoInstancesNeverDoubleHold(t *testing.T) {
	ce := newConcEnv(t, false)
	ce.cgw.submitDelay = 30 * time.Millisecond
	var ids []int64
	for i := 0; i < 6; i++ {
		ids = append(ids, ce.newJob(i))
	}
	_, stop1 := ce.start(EngineSizes{Job: 3})
	_, stop2 := ce.start(EngineSizes{Job: 3})
	waitFor(t, 10*time.Second, "全部成功", func() bool {
		for _, id := range ids {
			if ce.jobStatus(id) != model.JobSucceeded {
				return false
			}
		}
		return true
	})
	stop1()
	stop2()
	if ce.guard.violations.Load() != 0 || ce.cgw.noLease.Load() != 0 {
		t.Fatalf("租约违规=%d 无租约调用=%d", ce.guard.violations.Load(), ce.cgw.noLease.Load())
	}
	for _, id := range ids {
		if n := ce.gw.submitted(fmt.Sprintf("rpl-%d:seg/01/generate", id)); n != 1 {
			t.Fatalf("job %d 出片应只提交一次：%d", id, n)
		}
	}
	ce.assertMoneyAll(ids)
}

// 在途步骤的状态走批量读：调度器不再对出片步骤逐个 Get。
func TestEngineInFlightStepsUseBatchGet(t *testing.T) {
	ce := newConcEnv(t, false)
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, ce.newJob(i))
	}
	_, stop := ce.start(EngineSizes{})
	waitFor(t, 5*time.Second, "全部成功", func() bool {
		for _, id := range ids {
			if ce.jobStatus(id) != model.JobSucceeded {
				return false
			}
		}
		return true
	})
	stop()
	if ce.cgw.batchCalls.Load() == 0 || ce.cgw.batchIDs.Load() < 3 {
		t.Fatalf("应批量读在途调用：次数=%d ids=%d", ce.cgw.batchCalls.Load(), ce.cgw.batchIDs.Load())
	}
	if g := ce.cgw.gets.Load(); g != 0 {
		t.Fatalf("在途出片步骤不应再逐个 Get：%d", g)
	}
}

// PrefetchCalls 按网关上限分批（≤200），结果按 job 归属；批量读失败返回空（worker 回落单个 Get）。
type chunkGW struct {
	fakeGW
	mu     sync.Mutex
	chunks []int
	fail   bool
}

func (g *chunkGW) BatchGet(_ context.Context, ids []string) ([]aigateway.Call, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.chunks = append(g.chunks, len(ids))
	if g.fail {
		return nil, fmt.Errorf("boom")
	}
	out := make([]aigateway.Call, len(ids))
	for i, id := range ids {
		out[i] = aigateway.Call{ID: id, Status: aigateway.StatusRunning}
	}
	return out, nil
}

type fixedCalls struct {
	*memStore
	calls map[int64][]string
}

func (f fixedCalls) InFlightCalls(context.Context, []int64) (map[int64][]string, error) {
	return f.calls, nil
}

func TestPrefetchCallsChunksAndGroups(t *testing.T) {
	calls := map[int64][]string{}
	var jobs []int64
	for j := int64(1); j <= 5; j++ {
		jobs = append(jobs, j)
		for k := 0; k < 50; k++ {
			calls[j] = append(calls[j], fmt.Sprintf("c%d_%d", j, k))
		}
	}
	gw := &chunkGW{fakeGW: *newGW()}
	svc := New(DefaultConfig(), fixedCalls{newMemStore(time.Now), calls}, newLedger(), WithGateway(gw))
	got := svc.PrefetchCalls(context.Background(), jobs)
	sort.Ints(gw.chunks)
	if len(gw.chunks) != 2 || gw.chunks[0] != 50 || gw.chunks[1] != aigateway.MaxBatchGet {
		t.Fatalf("应分两批 200 + 50：%v", gw.chunks)
	}
	if len(got) != 5 || len(got[3]) != 50 || got[3]["c3_7"] == nil || got[3]["c4_7"] != nil {
		t.Fatalf("结果应按 job 归属：%d %d", len(got), len(got[3]))
	}
	ctx := WithPrefetchedCalls(context.Background(), got[3])
	if c := prefetchedCall(ctx, "c3_7"); c == nil || c.Status != aigateway.StatusRunning {
		t.Fatalf("ctx 里应能取到：%+v", c)
	}
	gw.fail = true
	if got := svc.PrefetchCalls(context.Background(), jobs); len(got) != 0 {
		t.Fatalf("批量读失败应返回空：%v", got)
	}
}

// 停机：有 worker 卡死且不理会取消（出片提交卡住、入库复制卡住），Stop 仍在硬上限内返回，行租约与入库认领都已释放。
func TestEngineStopBoundedWithStuckWorkers(t *testing.T) {
	ce := newConcEnv(t, true)
	ce.lib.ignoreCtx = true
	first := ce.newJob(1)
	cfg := EngineConfig{Interval: 50 * time.Millisecond, StopGrace: 200 * time.Millisecond, ReleaseTimeout: 200 * time.Millisecond}
	_, stop := ce.startCfg(cfg)
	waitFor(t, 5*time.Second, "第一条成功并卡在入库", func() bool { return ce.jobStatus(first) == model.JobSucceeded && ce.lib.entered.Load() >= 1 })
	ce.cgw.block = make(chan struct{})
	defer close(ce.lib.gate)
	defer close(ce.cgw.block)
	second := ce.newJob(2)
	waitFor(t, 5*time.Second, "第二条卡在出片提交", func() bool { return ce.cgw.entered.Load() >= 2 && ce.guard.holds(model.TableJob, second) })
	t0 := time.Now()
	stop()
	if d := time.Since(t0); d > cfg.MaxStopDuration()+300*time.Millisecond {
		t.Fatalf("停机应在上限内返回：用了 %s，上限 %s", d, cfg.MaxStopDuration())
	}
	if n := ce.guard.count(); n != 0 {
		t.Fatalf("停机后行租约应全部释放：还持有 %d", n)
	}
	ce.ops.mu.Lock()
	defer ce.ops.mu.Unlock()
	for _, d := range ce.ops.dels {
		if d.Status == model.DeliveryPending && d.LeaseToken != nil {
			t.Fatalf("停机后入库认领应已释放：delivery=%d", d.ID)
		}
	}
	if ce.guard.violations.Load() != 0 {
		t.Fatalf("租约违规=%d", ce.guard.violations.Load())
	}
}

// 租约丢失（被别人持有）：worker 的 ctx 立即取消，停止推进这一行，不再完成网关提交、不写回。
func TestEngineLeaseLostStopsWorker(t *testing.T) {
	ce := newConcEnv(t, false)
	ce.cgw.submitDelay = 10 * time.Second // 理会 ctx 的慢提交
	id := ce.newJob(1)
	eng, stop := ce.start(EngineSizes{})
	defer stop()
	waitFor(t, 3*time.Second, "进入出片提交", func() bool { return ce.cgw.entered.Load() >= 1 })
	ce.guard.steal(model.TableJob, id)
	waitFor(t, time.Second, "租约丢失后 worker 停手", func() bool { return eng.Busy(KindJob) == 0 })
	if n := ce.gw.submitted(fmt.Sprintf("rpl-%d:seg/01/generate", id)); n != 0 {
		t.Fatalf("租约丢了不应再完成提交：%d", n)
	}
	for _, st := range ce.steps(id) {
		if st.GatewayCallID != nil {
			t.Fatalf("租约丢了不应写回调用号：%+v", st)
		}
	}
	if ce.guard.violations.Load() != 0 {
		t.Fatalf("租约违规=%d", ce.guard.violations.Load())
	}
}

// 抢租约出错（库故障）：这一行本进程内退避，不会每次取数都空转。
func TestEngineLeaseErrorBacksOff(t *testing.T) {
	ce := newConcEnv(t, false)
	ce.guard.acquireErr = fmt.Errorf("db down")
	ce.newJob(1)
	_, stop := ce.startCfg(EngineConfig{Interval: 30 * time.Millisecond, LeaseErrBackoff: 5 * time.Second})
	time.Sleep(400 * time.Millisecond)
	stop()
	if n := ce.guard.attempts.Load(); n != 1 {
		t.Fatalf("出错后应退避，只试 1 次：%d", n)
	}
}

// 合并窗口：窗口内登记的多个 job 只触发一次 InFlightCalls + batchGet。
func TestCallBatcherMergesWithinWindow(t *testing.T) {
	gw := &chunkGW{fakeGW: *newGW()}
	calls := map[int64][]string{}
	for j := int64(1); j <= 10; j++ {
		calls[j] = []string{fmt.Sprintf("c%d", j)}
	}
	svc := New(DefaultConfig(), fixedCalls{newMemStore(time.Now), calls}, newLedger(), WithGateway(gw))
	b := newCallBatcher(100*time.Millisecond, time.Second)
	var futs []*callFuture
	for j := int64(1); j <= 10; j++ {
		futs = append(futs, b.add(svc, j))
	}
	for i, f := range futs {
		ctx := withCallFuture(context.Background(), f)
		if c := prefetchedCall(ctx, fmt.Sprintf("c%d", i+1)); c == nil {
			t.Fatalf("job %d 应拿到批量结果", i+1)
		}
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.chunks) != 1 || gw.chunks[0] != 10 {
		t.Fatalf("窗口内应只批量查一次：%v", gw.chunks)
	}
}
