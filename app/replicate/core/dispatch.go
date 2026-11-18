package core

import (
	"context"
	"sync"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 后台推进的「单行」入口与在途调用批量查状态（CONTRACT §AC）。
//
//   - ProcessAnalysis / ProcessJob / ProcessDelivery：推进一行（抢租约 → 推进 → 写回 next_run_at → 释放租约），
//     由并发调度器（scheduler.go 的 Engine）的各自工作池调用；TickGuarded / TickDeliveries 也改成逐个调用它们，语义不变。
//   - PrefetchCalls：调度器在派发一批到期 job 前，把这些 job 在途步骤（出片 / 写手）的网关调用号一次 batchGet（≤200 个一批），
//     结果按 job 放进各自 worker 的 ctx；pollStep / 写手推进先查 ctx，命中就不再逐个 Get。没命中（新提交 / 批量查失败 / 网关没返回）
//     照旧单个 Get——幂等键、终态处理完全不变，只是少了 N 次往返。
// ---------------------------------------------------------------------------------------------

// DueItem 一条到期行：id 与 next_run_at（nil = 从没排过，最先取）。next_run_at 用来打「到期 → 开始处理」的排队时长指标。
type DueItem struct {
	ID        int64
	NextRunAt *time.Time
}

// DueAtStore 可选：带 next_run_at 的到期查询（生产 repo 实现）。Store 没实现时退回 DueAnalyses / DueJobs，排队时长指标不打。
type DueAtStore interface {
	DueAnalysesAt(ctx context.Context, limit int) ([]DueItem, error)
	DueJobsAt(ctx context.Context, limit int) ([]DueItem, error)
}

// DueDeliveriesAtStore 可选：入库发件箱带 next_run_at 的到期查询（生产 repo 实现）。
type DueDeliveriesAtStore interface {
	DueDeliveriesAt(ctx context.Context, limit int) ([]DueItem, error)
}

// InFlightCallStore 可选：一批 job 里在途（RUNNING 且已有调用号）的步骤的网关调用号，job id → 调用号列表。
type InFlightCallStore interface {
	InFlightCalls(ctx context.Context, jobIDs []int64) (map[int64][]string, error)
}

// BatchGateway 可选：网关批量读（*aigateway.Client 满足，单次 ≤ aigateway.MaxBatchGet）。
type BatchGateway interface {
	BatchGet(ctx context.Context, ids []string) ([]aigateway.Call, error)
}

var _ BatchGateway = (*aigateway.Client)(nil)

func toItems(ids []int64) []DueItem {
	out := make([]DueItem, len(ids))
	for i, id := range ids {
		out[i] = DueItem{ID: id}
	}
	return out
}

// DueAnalysisItems 到期的分析（按 next_run_at 升序）。
func (s *Service) DueAnalysisItems(ctx context.Context, limit int) ([]DueItem, error) {
	if st, ok := s.store.(DueAtStore); ok {
		return st.DueAnalysesAt(ctx, limit)
	}
	ids, err := s.store.DueAnalyses(ctx, limit)
	return toItems(ids), err
}

// DueJobItems 到期的 job（按 next_run_at 升序）。
func (s *Service) DueJobItems(ctx context.Context, limit int) ([]DueItem, error) {
	if st, ok := s.store.(DueAtStore); ok {
		return st.DueJobsAt(ctx, limit)
	}
	ids, err := s.store.DueJobs(ctx, limit)
	return toItems(ids), err
}

// DueDeliveryItems 到期的入库（没装配入库返回空）。
func (s *Service) DueDeliveryItems(ctx context.Context, limit int) ([]DueItem, error) {
	if s.delivery == nil {
		return nil, nil
	}
	if st, ok := s.delivery.store.(DueDeliveriesAtStore); ok {
		return st.DueDeliveriesAt(ctx, limit)
	}
	ids, err := s.delivery.store.DueDeliveries(ctx, limit)
	return toItems(ids), err
}

// ProcessAnalysis 推进一条分析：抢租约（g 为 nil 不抢）→ AdvanceAnalysisHeld → 释放。
// 返回 false = 租约没抢到（别的实例 / 别的 worker 正在推进）或抢租约出错，本次没动这一行。
func (s *Service) ProcessAnalysis(ctx context.Context, id int64, g Guard) bool {
	lctx := ctx
	if g != nil {
		c, ok, err := g.Acquire(ctx, model.TableAnalysis, id)
		if err != nil || !ok {
			return false
		}
		defer g.Release(ctx, model.TableAnalysis, id)
		lctx = c
	}
	s.AdvanceAnalysisHeld(lctx, id)
	return true
}

// AdvanceAnalysisHeld 已持有租约时推进一条分析：同步阶段一口气推完 → 写回 next_run_at。
// ctx 应是租约派生的 ctx（租约丢失即取消，推进随之停手、不再提交或写库）。
func (s *Service) AdvanceAnalysisHeld(ctx context.Context, id int64) {
	delay := s.advanceAnalysisBurst(ctx, id)
	if ctx.Err() != nil {
		return // 租约丢了 / 停机：不写回，交给新的持有者
	}
	if err := s.store.RescheduleAnalysis(ctx, id, delay); err != nil {
		logx.WithContext(ctx).Errorf("replicate: 写分析 next_run_at 失败 analysis=%d: %v", id, err)
	}
}

// ProcessJob 推进一个 job：抢租约 → AdvanceJobHeld → 释放。返回 false = 租约没抢到或抢租约出错。
func (s *Service) ProcessJob(ctx context.Context, id int64, g Guard) bool {
	lctx := ctx
	if g != nil {
		c, ok, err := g.Acquire(ctx, model.TableJob, id)
		if err != nil || !ok {
			return false
		}
		defer g.Release(ctx, model.TableJob, id)
		lctx = c
	}
	s.AdvanceJobHeld(lctx, id)
	return true
}

// AdvanceJobHeld 已持有租约时推进一个 job：Advance → 写回 next_run_at（ctx 同 AdvanceAnalysisHeld）。
func (s *Service) AdvanceJobHeld(ctx context.Context, id int64) {
	err := s.Advance(ctx, id)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 推进 job=%d 失败: %v", id, err)
	}
	if rerr := s.store.RescheduleJob(ctx, id, s.jobDelay(ctx, id, err)); rerr != nil {
		logx.WithContext(ctx).Errorf("replicate: 写 job next_run_at 失败 job=%d: %v", id, rerr)
	}
}

// ProcessDelivery 处理一条入库（发件箱自己的 CAS 认领 + 令牌就是这一行的租约，见 runDelivery）。
func (s *Service) ProcessDelivery(ctx context.Context, id int64) {
	if s.delivery == nil {
		return
	}
	s.runDelivery(ctx, id)
}

type claimHookKey struct{}

// withClaimHook 调度器登记入库认领令牌：runDelivery 认领成功后回调（停机时据此统一释放认领，不必等 ClaimTTL 过期）。
func withClaimHook(ctx context.Context, f func(token string)) context.Context {
	return context.WithValue(ctx, claimHookKey{}, f)
}

func claimed(ctx context.Context, token string) {
	if f, _ := ctx.Value(claimHookKey{}).(func(string)); f != nil {
		f(token)
	}
}

// ReleaseDeliveryClaim 放掉一条入库认领（CAS 令牌：已被别人接手 / 已完成的不受影响），下次到期即可被任何实例重新认领。
func (s *Service) ReleaseDeliveryClaim(ctx context.Context, id int64, token string) error {
	if s.delivery == nil || token == "" {
		return nil
	}
	_, err := s.delivery.store.FinishDelivery(ctx, id, token, map[string]any{"lease_token": nil, "lease_until": nil}, 0)
	return err
}

// ---------------------------------------------------------------- 在途调用批量查状态

// callFuture 一批在途调用状态（调用号 → Call），done 关闭后 calls 可读（只读共享）。
type callFuture struct {
	done  chan struct{}
	calls map[string]*aigateway.Call
}

type prefetchKey struct{}

// WithPrefetchedCalls 把已查好的调用状态放进 ctx（已就绪的 future），供推进时代替逐个 Get。
func WithPrefetchedCalls(ctx context.Context, calls map[string]*aigateway.Call) context.Context {
	if len(calls) == 0 {
		return ctx
	}
	f := &callFuture{done: make(chan struct{}), calls: calls}
	close(f.done)
	return context.WithValue(ctx, prefetchKey{}, f)
}

func withCallFuture(ctx context.Context, f *callFuture) context.Context {
	return context.WithValue(ctx, prefetchKey{}, f)
}

// prefetchedCall 取 ctx 里批量查到的结果：future 还没好就等（批量查有硬时限，必然会好），ctx 结束则放弃。
func prefetchedCall(ctx context.Context, id string) *aigateway.Call {
	f, _ := ctx.Value(prefetchKey{}).(*callFuture)
	if f == nil {
		return nil
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil
	}
	if c := f.calls[id]; c != nil {
		cc := *c // 副本：同一 map 只读共享
		return &cc
	}
	return nil
}

// pollCall 查一条在途调用：ctx 里有批量查到的结果就用它（PollWait>0 的长轮询配置下只用终态结果，非终态仍走长轮询），否则单个 Get。
func (s *Service) pollCall(ctx context.Context, id string) (*aigateway.Call, error) {
	if c := prefetchedCall(ctx, id); c != nil && (c.IsTerminal() || s.cfg.PollWait <= 0) {
		return c, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.gw.Get(ctx, id, s.cfg.PollWait)
}

// callBatcher 合并窗口：窗口内各 worker 登记的 job 合成一次 InFlightCalls + batchGet（每窗口至多一次），
// 不在取数协程里同步做，也不会因为积压时频繁补位而每 200ms 多查一次。
type callBatcher struct {
	window, timeout time.Duration
	mu              sync.Mutex
	cur             *callBatch
}

type callBatch struct {
	svc  *Service
	futs map[int64]*callFuture
}

func newCallBatcher(window, timeout time.Duration) *callBatcher {
	return &callBatcher{window: window, timeout: timeout}
}

// add 登记一个已持有租约的 job，返回它的 future（窗口到点后统一查）。
func (b *callBatcher) add(svc *Service, id int64) *callFuture {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		b.cur = &callBatch{futs: map[int64]*callFuture{}}
		time.AfterFunc(b.window, b.flush)
	}
	b.cur.svc = svc // 用最新的内核（运行参数重建不影响批量读）
	f := b.cur.futs[id]
	if f == nil {
		f = &callFuture{done: make(chan struct{})}
		b.cur.futs[id] = f
	}
	return f
}

func (b *callBatcher) flush() {
	b.mu.Lock()
	bt := b.cur
	b.cur = nil
	b.mu.Unlock()
	if bt == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("replicate: 批量查调用状态 panic（这批逐个查）: %v", r)
		}
		for _, f := range bt.futs {
			select {
			case <-f.done:
			default:
				close(f.done)
			}
		}
	}()
	ids := make([]int64, 0, len(bt.futs))
	for id := range bt.futs {
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	res := bt.svc.PrefetchCalls(ctx, ids)
	for id, f := range bt.futs {
		f.calls = res[id]
		close(f.done)
	}
}

// PrefetchCalls 批量查这批 job 的在途调用状态，返回 job id → (调用号 → Call)。
// 网关不支持批量 / Store 不支持列调用号 / 查询失败时返回 nil（worker 照旧逐个 Get，不影响正确性）。
func (s *Service) PrefetchCalls(ctx context.Context, jobIDs []int64) map[int64]map[string]*aigateway.Call {
	bg, ok := s.gw.(BatchGateway)
	if !ok || s.gw == nil || len(jobIDs) == 0 {
		return nil
	}
	cs, ok := s.store.(InFlightCallStore)
	if !ok {
		return nil
	}
	byJob, err := cs.InFlightCalls(ctx, jobIDs)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 列在途调用号失败（本轮逐个查） 判据=PREFETCH_LIST_FAILED: %v", err)
		return nil
	}
	owner := map[string]int64{}
	var ids []string
	for jid, calls := range byJob {
		for _, c := range calls {
			if c == "" {
				continue
			}
			if _, dup := owner[c]; !dup {
				owner[c] = jid
				ids = append(ids, c)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := map[int64]map[string]*aigateway.Call{}
	for i := 0; i < len(ids); i += aigateway.MaxBatchGet {
		chunk := ids[i:min(i+aigateway.MaxBatchGet, len(ids))]
		calls, err := bg.BatchGet(ctx, chunk)
		if err != nil {
			engineBatchGet.Inc("error")
			logx.WithContext(ctx).Infof("replicate: 批量查调用状态失败（这批逐个查） 判据=PREFETCH_BATCH_FAILED 个数=%d: %v", len(chunk), err)
			continue
		}
		engineBatchGet.Inc("ok")
		for k := range calls {
			c := calls[k]
			jid, ok := owner[c.ID]
			if !ok {
				continue
			}
			if out[jid] == nil {
				out[jid] = map[string]*aigateway.Call{}
			}
			out[jid][c.ID] = &c
		}
	}
	return out
}
