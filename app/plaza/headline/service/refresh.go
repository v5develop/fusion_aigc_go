package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"

	"github.com/zeromicro/go-zero/core/logx"
)

// RefreshOutcome 一次刷新的结果（运营写后 / 立即刷新的响应）。
type RefreshOutcome struct {
	Version   int64                     `json:"version"`
	Refreshed bool                      `json:"refreshed"`
	Diff      map[string]algo.ScopeDiff `json:"diff,omitempty"`
}

// computation 一次计算的产物与元信息（刷新与试算共用）。
type computation struct {
	cfg        algo.Config
	result     algo.RefreshResult
	source     string
	now        time.Time
	prev       map[string][]algo.PrevItem
	prevVer    int64
	catalog    *store.Catalog
	durationMs int
}

// compute 读参数 → 目录 → 信号（共享一次）→ 规则与抑制 → 上一版 → 逐范围计算。cfgOverride 非 nil 时用它（试算）。
func (s *Service) compute(ctx context.Context, cfgOverride *algo.Config, onlyScopes map[string]bool) (*computation, error) {
	start := time.Now()
	now := s.now()
	cfg, _, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfgOverride != nil {
		cfg = *cfgOverride
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("读目录: %w", err)
	}
	sigs, source, err := s.readSignals(ctx, now, cfg.Global)
	if err != nil {
		return nil, fmt.Errorf("读信号: %w", err)
	}
	rules, blocks, err := s.rulesAndBlocks(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("读规则: %w", err)
	}
	prev, prevVer, err := s.latestItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("读上一版: %w", err)
	}
	in := algo.RefreshInput{
		Config: cfg, Scopes: cat.Scopes, Templates: cat.AlgoTemplates(now), Signals: sigs, Now: now,
		Prev: prev, OnlyScopes: onlyScopes, IncludeGlobal: len(onlyScopes) == 0 || onlyScopes[algo.ScopeAll],
	}
	for _, r := range rules {
		in.Rules = append(in.Rules, r.Algo())
	}
	for _, b := range blocks {
		in.Blocks = append(in.Blocks, b.Algo())
	}
	res := algo.Refresh(in)
	return &computation{cfg: cfg, result: res, source: source, now: now, prev: prev, prevVer: prevVer, catalog: cat,
		durationMs: int(time.Since(start).Milliseconds())}, nil
}

// latestItems 最新版本的条目按范围分组。
func (s *Service) latestItems(ctx context.Context) (map[string][]algo.PrevItem, int64, error) {
	snap, err := s.repo.LatestSnapshot(ctx)
	if err != nil || snap == nil {
		return map[string][]algo.PrevItem{}, 0, err
	}
	rows, err := s.repo.Items(ctx, snap.ID)
	if err != nil {
		return nil, 0, err
	}
	out := map[string][]algo.PrevItem{}
	for _, r := range rows {
		out[r.ScopeCode] = append(out[r.ScopeCode], algo.PrevItem{TemplateID: r.PlazaTemplateID, Rank: r.RankNo, Source: r.Source, EnteredAt: r.EnteredAt.Time})
	}
	return out, snap.ID, nil
}

// lockTTL 刷新锁 TTL；持锁期间每 lockTTL/3 续期一次（M7）。变量仅为单测缩短。
var lockTTL = 30 * time.Second

// refreshLock 一次持有的刷新锁。Redis 锁带令牌：续期、写快照与写缓存前都比对令牌；进程内锁（无 Redis，仅开发）恒视为持有。
type refreshLock struct {
	s       *Service
	token   string // 空 = 进程内锁
	lost    atomic.Bool
	stop    chan struct{}
	stopped chan struct{}
}

// fence 写缓存用的令牌比对（进程内锁返回 nil）。
func (l *refreshLock) fence() *signals.Fence {
	if l == nil || l.token == "" {
		return nil
	}
	return &signals.Fence{Key: signals.LockKey, Token: l.token}
}

// stillHeld 写快照前确认锁仍属于自己。Redis 出错按「已丢锁」处理（宁可放弃这一版，也不和别的副本并发写）。
func (l *refreshLock) stillHeld(ctx context.Context) (bool, string) {
	if l == nil || l.token == "" {
		return true, ""
	}
	if l.lost.Load() {
		return false, "续期时发现锁已不属于本次刷新"
	}
	ok, err := l.s.sig.HoldsLock(ctx, signals.LockKey, l.token)
	if err != nil {
		return false, fmt.Sprintf("比对锁令牌出错: %v", err)
	}
	if !ok {
		return false, "锁令牌已不匹配（锁过期后被其他副本拿走）"
	}
	return true, ""
}

// renewLoop 每 lockTTL/3 续期；续不上（令牌不匹配）记丢锁并停止。
func (l *refreshLock) renewLoop() {
	defer close(l.stopped)
	t := time.NewTicker(lockTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ok, err := l.s.sig.RenewLock(ctx, signals.LockKey, l.token, lockTTL)
		cancel()
		if err != nil {
			logx.Errorf("plaza headline: 刷新锁续期出错（下次再试，写入前仍会比对令牌）: %v", err)
			continue
		}
		if !ok {
			l.lost.Store(true)
			logx.Errorf("WARN plaza headline: 刷新锁续期失败——判据：令牌已不匹配（锁已过期并被其他副本拿走），本次刷新结果将放弃")
			return
		}
	}
}

func (l *refreshLock) release() {
	if l.token == "" {
		l.s.refreshMu.Unlock()
		return
	}
	close(l.stop)
	<-l.stopped
	l.s.sig.Unlock(context.Background(), signals.LockKey, l.token)
}

// RefreshNow 所有刷新的唯一入口（定时 / 运营写后 / 立即刷新），任意副本可调用。
//
// Redis 锁 aigc:plaza:headlines:refresh:lock 互斥（TTL 30s，持锁期间每 10s 续期；写快照与写缓存前比对令牌，
// 锁已不属于自己就放弃本次结果）。拿不到：运营触发置脏标记并最多等 3 秒（运行中的刷新结束后会再跑一次），定时触发直接放弃本轮。
// Redis 锁出错时跳过本轮并 WARN（不退回进程内锁：进程内锁挡不住其他副本）；只有没配 Redis（仅开发）才用进程内锁。
// 任一范围计算异常整版回滚（不写新版本，沿用上一版）；单个范围成员为空只让它输出空榜。
func (s *Service) RefreshNow(ctx context.Context, trigger string, operator int64) (*RefreshOutcome, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	lk, ok, err := s.lock(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("WARN plaza headline: 跳过本轮刷新 trigger=%s——判据：Redis 刷新锁不可用（不退回进程内锁，避免多副本并发写版本）: %v", trigger, err)
		return s.notRefreshed(ctx), nil
	}
	if !ok {
		if trigger == TriggerScheduled || trigger == TriggerStartup {
			return &RefreshOutcome{Refreshed: false}, nil
		}
		_ = s.sig.MarkDirty(ctx)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			if lk, ok, err = s.lock(ctx); err != nil {
				logx.WithContext(ctx).Errorf("WARN plaza headline: 跳过本轮刷新 trigger=%s——判据：等待期间 Redis 刷新锁不可用: %v", trigger, err)
				return s.notRefreshed(ctx), nil
			} else if ok {
				break
			}
		}
		if !ok {
			return s.notRefreshed(ctx), nil
		}
	}
	defer lk.release()
	out, err := s.refreshLocked(ctx, trigger, operator, lk)
	// 刷新期间又有人置了脏标记：再跑一次（规则已落库，这次会带上）
	if err == nil && out.Refreshed {
		if dirty, _ := s.sig.TakeDirty(ctx); dirty {
			if again, err2 := s.refreshLocked(ctx, trigger, operator, lk); err2 == nil && again.Refreshed {
				out = again
			}
		}
	}
	return out, err
}

// notRefreshed 没刷新时的响应：带当前最新版本号。
func (s *Service) notRefreshed(ctx context.Context) *RefreshOutcome {
	out := &RefreshOutcome{Refreshed: false}
	if snap, _ := s.repo.LatestSnapshot(ctx); snap != nil {
		out.Version = snap.ID
	}
	return out
}

// lock 抢刷新锁。有 Redis：Redis 锁（出错返回 err，调用方跳过本轮）；没配 Redis：进程内锁。
func (s *Service) lock(ctx context.Context) (*refreshLock, bool, error) {
	if s.sig.Enabled() {
		token, ok, err := s.sig.TryLock(ctx, signals.LockKey, lockTTL)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		l := &refreshLock{s: s, token: token, stop: make(chan struct{}), stopped: make(chan struct{})}
		go l.renewLoop()
		return l, true, nil
	}
	if !s.refreshMu.TryLock() {
		return nil, false, nil
	}
	return &refreshLock{s: s}, true, nil
}

func (s *Service) refreshLocked(ctx context.Context, trigger string, operator int64, lk *refreshLock) (*RefreshOutcome, error) {
	c, err := s.compute(ctx, nil, nil)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 刷新失败（沿用上一版） trigger=%s: %v", trigger, err)
		return nil, err
	}
	paramsJSON, _ := c.cfg.CanonicalJSON()
	statsJSON, _ := json.Marshal(scopeStats(c.result))
	var items []algo.Item
	for _, sr := range c.result.Scopes {
		items = append(items, sr.Items...)
	}
	dur := c.durationMs
	snap := &store.Snapshot{
		ComputedAt: store.CSTTime{Time: c.now}, TriggerType: trigger, ParamsHash: c.cfg.Hash(), ParamsJSON: paramsJSON,
		SignalSource: c.source, WindowStart: store.CSTTime{Time: c.now.Add(-7 * 24 * time.Hour)}, WindowEnd: store.CSTTime{Time: c.now},
		CandidateCount: c.result.CandidateCount, ScopeCount: len(c.result.Scopes), ItemCount: len(items), ScopeStats: statsJSON, DurationMs: &dur,
	}
	if operator > 0 {
		snap.OperatorID = &operator
	}
	if held, why := lk.stillHeld(ctx); !held {
		logx.WithContext(ctx).Errorf("WARN plaza headline: 放弃本次刷新结果（不写快照）trigger=%s——判据：%s", trigger, why)
		return s.notRefreshed(ctx), nil
	}
	version, err := s.repo.InsertSnapshot(ctx, snap, items)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 写快照失败（沿用上一版） trigger=%s: %v", trigger, err)
		return nil, err
	}
	s.writeLatestCache(ctx, version, c, lk.fence())
	out := &RefreshOutcome{Version: version, Refreshed: true, Diff: map[string]algo.ScopeDiff{}}
	for _, sr := range c.result.Scopes {
		if d := algo.Diff(c.prev[sr.Scope], sr.Items); !d.Empty() {
			out.Diff[sr.Scope] = d
		}
	}
	s.logRefresh(ctx, version, trigger, c, len(items))
	return out, nil
}

// logRefresh 一行总览 + 每个范围一行摘要（带排除判据与数量，沿用「拒收日志要带判据」）。
func (s *Service) logRefresh(ctx context.Context, version int64, trigger string, c *computation, items int) {
	fill := 0
	for _, sr := range c.result.Scopes {
		fill += sr.Stats.Fill
	}
	logx.WithContext(ctx).Infof("plaza headline: version=%d trigger=%s 信号源=%s 范围=%d 候选=%d 条目=%d 补位=%d 耗时=%dms",
		version, trigger, c.source, len(c.result.Scopes), c.result.CandidateCount, items, fill, c.durationMs)
	for _, sr := range c.result.Scopes {
		keys := make([]string, 0, len(sr.Stats.Skip))
		for k := range sr.Stats.Skip {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%d", k, sr.Stats.Skip[k])
		}
		logx.WithContext(ctx).Infof("plaza headline: scope=%s 合格=%d AUTO=%d 人工=%d 补位=%d 排除{%s}",
			sr.Scope, sr.Stats.Eligible, sr.Stats.Auto, sr.Stats.Manual, sr.Stats.Fill, strings.Join(parts, ","))
	}
}

func scopeStats(r algo.RefreshResult) map[string]algo.ScopeStats {
	out := make(map[string]algo.ScopeStats, len(r.Scopes))
	for _, sr := range r.Scopes {
		out[sr.Scope] = sr.Stats
	}
	return out
}

// latestPayload 每个行业范围的最新榜单缓存（用户端读；不含分数明细）。
type latestPayload struct {
	Version     int64       `json:"version"`
	ComputedAt  time.Time   `json:"computedAt"`
	WindowStart time.Time   `json:"windowStart"`
	WindowEnd   time.Time   `json:"windowEnd"`
	Scope       string      `json:"scope"`
	Items       []algo.Item `json:"items"`
}

func (s *Service) writeLatestCache(ctx context.Context, version int64, c *computation, fence *signals.Fence) {
	if !s.sig.Enabled() {
		return
	}
	payloads := map[string][]byte{}
	for _, sr := range c.result.Scopes {
		if sr.Scope == algo.ScopeAll {
			continue
		}
		items := make([]algo.Item, len(sr.Items))
		for i, it := range sr.Items {
			it.Detail = nil
			it.Score = nil
			items[i] = it
		}
		b, _ := json.Marshal(latestPayload{Version: version, ComputedAt: c.now, WindowStart: c.now.Add(-7 * 24 * time.Hour), WindowEnd: c.now, Scope: sr.Scope, Items: items})
		payloads[sr.Scope] = b
	}
	if _, err := s.sig.SetLatest(ctx, payloads, version, s.conf.latestTTL(), fence); errors.Is(err, signals.ErrLockLost) {
		logx.WithContext(ctx).Errorf("WARN plaza headline: 放弃写榜单缓存 version=%d——判据：写缓存前比对锁令牌不匹配（锁已被其他副本拿走，由它写更新的版本；用户端暂回落读库）", version)
	} else if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 写榜单缓存失败（用户端回落读库）: %v", err)
	}
}

// PreviewResult 试算结果：某范围的试算榜单 + 相对当前版的 diff。
type PreviewResult struct {
	Scope      string          `json:"scope"`
	ParamsHash string          `json:"paramsHash"`
	Items      []algo.Item     `json:"items"`
	Stats      algo.ScopeStats `json:"stats"`
	Diff       algo.ScopeDiff  `json:"diff"`
	BaseVer    int64           `json:"baseVersion"`
}

// Preview 只算不写：可带参数覆盖（含行业覆盖）与范围（缺省 ALL）。
func (s *Service) Preview(ctx context.Context, override *algo.Config, scope string) (*PreviewResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if scope == "" {
		scope = algo.ScopeAll
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if scope != algo.ScopeAll {
		if _, ok := cat.Scope(scope); !ok {
			return nil, errIndustryNotFound()
		}
	}
	if override != nil {
		if errs := override.Validate(cat.ScopeCodes()); len(errs) > 0 {
			return nil, paramsInvalid(errs)
		}
	}
	c, err := s.compute(ctx, override, map[string]bool{scope: true})
	if err != nil {
		return nil, err
	}
	sr, _ := c.result.Scope(scope)
	return &PreviewResult{Scope: scope, ParamsHash: c.cfg.Hash(), Items: sr.Items, Stats: sr.Stats, Diff: algo.Diff(c.prev[scope], sr.Items), BaseVer: c.prevVer}, nil
}

// TriggerAsync 网关读到「该范围在最新版本里不存在」（刚新增的行业）时异步触发一次刷新：
// 只置脏标记，由后台任务的租约主在下一个 tick 刷新；同一范围 1 分钟内只置一次。
func (s *Service) TriggerAsync(ctx context.Context, scope string) {
	now := s.now()
	if v, ok := s.triggerOnce.Load(scope); ok && now.Sub(v.(time.Time)) < time.Minute {
		return
	}
	s.triggerOnce.Store(scope, now)
	if err := s.sig.MarkDirty(ctx); err != nil && !errors.Is(err, signals.ErrNoRedis) {
		logx.WithContext(ctx).Errorf("plaza headline: 置刷新标记失败: %v", err)
	}
}
