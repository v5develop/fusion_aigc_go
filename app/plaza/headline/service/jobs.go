package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/lease"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	leaseTTL      = 60 * time.Second
	leaseRenew    = 20 * time.Second
	tickEvery     = 5 * time.Second
	keepLatestVer = 20
)

// Jobs 头条后台任务（go-zero service.Service）：用 afv_plaza_headline_ctl 的行租约选主（TTL 60s、每 20s 续租、丢租约立即停），
// 租约主在同一 goroutine 串行执行：摄入（15s）、刷新（10min，或有脏标记时立即）、校准（每小时第 5 分钟）、保留清理（每日 03:30）。
// 首次选主 30 秒后出第一版榜单；Redis 桶为空而 DB 有数据时先校准再刷新。
type Jobs struct {
	s      *Service
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	start  sync.Once

	leader      bool
	leaderSince time.Time
	lastRenew   time.Time
	lastIngest  time.Time
	lastRefresh time.Time
	lastCalib   time.Time
	lastClean   time.Time
	failures    int
	started     atomic.Bool
}

// NewJobs 建后台任务。
func NewJobs(s *Service) *Jobs {
	ctx, cancel := context.WithCancel(context.Background())
	return &Jobs{s: s, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Start 阻塞运行直到 Stop。
func (j *Jobs) Start() {
	j.started.Store(true)
	defer close(j.done)
	if err := j.s.ready(); err != nil {
		logx.Info("plaza headline: 未配置数据库，后台任务不启动")
		<-j.ctx.Done()
		return
	}
	logx.Info("plaza headline: 后台任务已启动（等待选主）")
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		j.Tick(j.ctx)
		select {
		case <-j.ctx.Done():
			if j.leader {
				_ = lease.Release(context.Background(), j.s.repo.DB, store.TableCtl, 1, lease.Owner())
			}
			return
		case <-t.C:
		}
	}
}

// Stop 停止并等待退出。
func (j *Jobs) Stop() {
	j.cancel()
	if j.started.Load() {
		<-j.done
	}
}

// Tick 推进一轮（单测可直接调用）。
func (j *Jobs) Tick(ctx context.Context) {
	now := j.s.now()
	if !j.ensureLeader(ctx, now) {
		return
	}
	if now.Sub(j.lastIngest) >= j.s.conf.ingestInterval() {
		j.lastIngest = now
		j.ingest(ctx)
	}
	// 校准：每小时第 5 分钟（或成为租约主后 Redis 为空时立即）
	if j.lastCalib.IsZero() || (now.Minute() >= 5 && now.Truncate(time.Hour).After(j.lastCalib.Truncate(time.Hour))) {
		if j.lastCalib.IsZero() {
			if has, err := j.s.sig.HasAnyBucket(ctx, now); err == nil && !has {
				j.calibrate(ctx, now)
			}
			j.lastCalib = now
		} else {
			j.calibrate(ctx, now)
		}
	}
	first := j.lastRefresh.IsZero() && now.Sub(j.leaderSince) >= time.Duration(j.s.conf.FirstRefreshDelaySeconds)*time.Second
	due := !j.lastRefresh.IsZero() && now.Sub(j.lastRefresh) >= j.s.conf.refreshInterval()
	dirty := false
	if !first && !due && !j.lastRefresh.IsZero() {
		dirty, _ = j.s.sig.TakeDirty(ctx)
	}
	if first || due || dirty {
		trigger := TriggerScheduled
		switch {
		case first:
			trigger = TriggerStartup
		case dirty:
			trigger = TriggerOps
		}
		j.lastRefresh = now
		if _, err := j.s.RefreshNow(ctx, trigger, 0); err != nil {
			j.failures++
			if j.failures >= 3 {
				logx.WithContext(ctx).Errorf("plaza headline: 连续 %d 次刷新失败，榜单停在上一版", j.failures)
			}
		} else {
			j.failures = 0
		}
	}
	// 保留清理：每日 03:30 之后跑一次
	local := now.In(algo.CST)
	if (local.Hour() > 3 || (local.Hour() == 3 && local.Minute() >= 30)) && !sameDay(j.lastClean, now) {
		j.lastClean = now
		j.cleanup(ctx, now)
	}
}

func sameDay(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	return a.In(algo.CST).Format("20060102") == b.In(algo.CST).Format("20060102")
}

// ensureLeader 抢 / 续租约；丢租约立即停手。
func (j *Jobs) ensureLeader(ctx context.Context, now time.Time) bool {
	db, owner := j.s.repo.DB, lease.Owner()
	if j.leader {
		if now.Sub(j.lastRenew) < leaseRenew {
			return true
		}
		ok, err := lease.Extend(ctx, db, store.TableCtl, 1, owner, leaseTTL)
		if err != nil || !ok {
			logx.WithContext(ctx).Errorf("plaza headline: 租约丢失，停止后台任务（err=%v）", err)
			j.leader = false
			return false
		}
		j.lastRenew = now
		return true
	}
	if _, err := j.s.repo.Ctl(ctx); err != nil { // 控制行不存在时补建
		logx.WithContext(ctx).Errorf("plaza headline: 读控制行失败（迁移 Go_V1.0.35 未执行？）: %v", err)
		return false
	}
	ok, err := lease.Acquire(ctx, db, store.TableCtl, 1, owner, leaseTTL)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 抢租约失败: %v", err)
		return false
	}
	if ok {
		logx.WithContext(ctx).Infof("plaza headline: 成为租约主 owner=%s", owner)
		j.leader, j.leaderSince, j.lastRenew = true, now, now
		j.lastRefresh, j.lastCalib = time.Time{}, time.Time{}
	}
	return ok
}

// ingest 增量读 afv_agent_turn（id > 水位，滞后 5s）灌 Redis 小时桶；追平或读满 10 批即止。
func (j *Jobs) ingest(ctx context.Context) {
	if !j.s.sig.Enabled() {
		return // 没有 Redis：刷新直接读 DB 聚合
	}
	ctl, err := j.s.repo.Ctl(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 读水位失败: %v", err)
		return
	}
	cfg, _, _ := j.s.LoadConfig(ctx)
	wm := ctl.IngestLastID
	if wm == 0 {
		// 首次：从当前最大 id 起，历史交给校准（不从 0 全表扫）
		maxID, err := j.s.repo.MaxTurnID(ctx)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza headline: 初始化水位失败: %v", err)
			return
		}
		if maxID == 0 {
			return
		}
		if ok, err := j.s.repo.AdvanceWatermark(ctx, 0, maxID); err != nil || !ok {
			return
		}
		logx.WithContext(ctx).Infof("plaza headline: 摄入水位初始化为 %d，历史由校准补齐", maxID)
		j.calibrate(ctx, j.s.now())
		return
	}
	batch := j.s.conf.IngestBatch
	if batch <= 0 {
		batch = 500
	}
	lag := time.Duration(j.s.conf.IngestLagSeconds) * time.Second
	for i := 0; i < 10; i++ {
		turns, err := j.s.repo.TurnsAfter(ctx, wm, lag, batch)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza headline: 读做同款轮次失败: %v", err)
			return
		}
		if len(turns) == 0 {
			return
		}
		counted, err := j.s.sig.Ingest(ctx, turns, cfg.Global.PerUserDailyCap, cfg.Global.RapidDedupeSeconds)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza headline: 写 Redis 计数失败（水位不推进，下轮重读）: %v", err)
			return
		}
		next := turns[len(turns)-1].RowID
		if ok, err := j.s.repo.AdvanceWatermark(ctx, wm, next); err != nil || !ok {
			logx.WithContext(ctx).Errorf("plaza headline: 推进水位失败 %d→%d（ok=%v err=%v）", wm, next, ok, err)
			return
		}
		logx.WithContext(ctx).Debugf("plaza headline: 摄入 %d 行，计入 %d，水位 %d→%d", len(turns), counted, wm, next)
		wm = next
		if len(turns) < batch {
			return
		}
	}
}

// calibrate 用 DB 聚合覆盖过去 14 天已完成的小时桶并补 HLL；记录漂移。
//
// 窗口起点对齐 CST 日界（每日封顶按 CST 自然日，从半天开始会让首日漏掉更早的尝试、多算，L10）；
// 只聚合 id ≤ 已摄入水位的行（L11）：水位之后的行还没摄入、下一轮摄入会把它们加进小时桶，
// 校准若也算上就会在摄入时再计一次。
func (j *Jobs) calibrate(ctx context.Context, now time.Time) {
	j.lastCalib = now
	if !j.s.sig.Enabled() {
		return
	}
	cfg, _, _ := j.s.LoadConfig(ctx)
	ctl, err := j.s.repo.Ctl(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 校准读水位失败（本轮不校准）: %v", err)
		return
	}
	from := signalWindowStart(now)
	// 水位 0 = 摄入还没初始化：初始化时会直接跳到当时的最大 id，不会重读这些行，所以此时全量聚合不会重复
	turns, err := j.s.repo.TurnsBetween(ctx, from, now, ctl.IngestLastID)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 校准读库失败: %v", err)
		return
	}
	counts := algo.CountTurns(turns, cfg.Global.PerUserDailyCap, cfg.Global.RapidDedupeSeconds)
	drift, err := j.s.sig.Calibrate(ctx, counts, from, now)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 校准写 Redis 失败: %v", err)
		return
	}
	_ = j.s.repo.MarkCalibrated(ctx, now)
	if drift > 0 {
		logx.WithContext(ctx).Infof("plaza headline: 小时校准完成，漂移 Σ|redis−db|=%d（轮次 %d，水位 %d）", drift, len(turns), ctl.IngestLastID)
	}
	if drift > 100 {
		logx.WithContext(ctx).Errorf("plaza headline: 校准漂移偏大 %d，检查摄入是否掉队 / DSN 时区", drift)
	}
}

// cleanup 保留策略：24h 内每版保留、之后每小时第一版、超期整版删除、至少留 20 版。失败不影响服务。
func (j *Jobs) cleanup(ctx context.Context, now time.Time) {
	cfg, _, _ := j.s.LoadConfig(ctx)
	metas, err := j.s.repo.SnapshotMetas(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 读快照列表失败: %v", err)
		return
	}
	del := algo.ExpiredSnapshots(metas, now, cfg.Global.SnapshotRetentionDays, keepLatestVer)
	if len(del) == 0 {
		return
	}
	if err := j.s.repo.DeleteSnapshots(ctx, del); err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 清理快照失败: %v", err)
		return
	}
	logx.WithContext(ctx).Infof("plaza headline: 清理过期快照 %d 版", len(del))
}
