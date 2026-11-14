// Package service 「本周头条」的编排：目录缓存、信号读取（Redis，失败降级 DB）、整版刷新、用户端读取、运营接口、后台任务。
//
// 与行业 / 模板模块（app/plaza/core）的边界：本包只读它们的表（store.LoadCatalog，可替换为 CatalogSource 的其他实现），
// 不 import 它们的包、不改它们的文件；目录版本号沿用同一个 Redis 键（aigc:plaza:catalog:ver），后台写模板 / 行业后
// 由它们递增，这里每 2 秒最多比对一次并重载。
//
// 宿主：与 app/plaza/core 一样由 gateway-api（用户端读）与 admin-api（运营接口 + 后台任务）进程内直接使用；
// 后台任务用 afv_plaza_headline_ctl 的行租约选主，多副本只有一个在跑。
package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 细分 reason（码不变，只增）。INDUSTRY_NOT_FOUND 与行业模块同名同码。
const (
	ReasonIndustryNotFound       = "INDUSTRY_NOT_FOUND"             // 4001
	ReasonScopeNotEditable       = "HEADLINE_SCOPE_NOT_EDITABLE"    // 1003：对保留范围 ALL 写规则 / 抑制
	ReasonTemplateNotInScope     = "HEADLINE_TEMPLATE_NOT_IN_SCOPE" // 1003：显式范围不包含该模板
	ReasonTemplateSuppressed     = "HEADLINE_TEMPLATE_SUPPRESSED"   // 4002：该范围已取消 / 屏蔽，不能再推荐
	ReasonDuplicate              = "HEADLINE_DUPLICATE"             // 4002：同模板同范围窗口重叠的生效规则已存在
	ReasonParamsInvalid          = "HEADLINE_PARAMS_INVALID"        // 1003：参数越界（data 里带字段）
	ReasonParamsConflict         = "HEADLINE_PARAMS_CONFLICT"       // 4002：参数已被他人修改
	ReasonSnapshotExpired        = "HEADLINE_SNAPSHOT_EXPIRED"      // 4001：超过保留期
	ReasonHeadlineTemplateAbsent = "HEADLINE_TEMPLATE_NOT_FOUND"    // 4001：模板不存在 / 不可见
	ReasonNotReady               = "PLAZA_NOT_READY"                // 7003：库没配
)

// 触发方式（snapshot.trigger_type）。
const (
	TriggerScheduled  = "SCHEDULED"
	TriggerOps        = "OPS"
	TriggerRefreshNow = "REFRESH_NOW"
	TriggerStartup    = "STARTUP"
)

// 信号来源（snapshot.signal_source）。
const (
	SourceRedis = "REDIS"
	SourceDB    = "DB"
)

// CatalogSource 目录来源（默认 store.Repo.LoadCatalog 直读行业 / 模板表；行业模块提供更快的快照时可替换）。
type CatalogSource interface {
	LoadCatalog(ctx context.Context, now time.Time) (*store.Catalog, error)
}

// Conf 头条配置（挂在 gateway-api / admin-api 配置的 PlazaHeadline 下，全部可选）。
type Conf struct {
	// Jobs 本进程是否跑后台任务（摄入 / 刷新 / 校准 / 清理）。默认关；只在 admin-api 生效（与广场模板域的后台任务同一进程，
	// 多副本靠 afv_plaza_headline_ctl 行租约选主），gateway-api 恒不跑（即使配了 true）。上线时在 admin-api 打开。
	Jobs bool `json:",default=false"`
	// RefreshIntervalSeconds 榜单刷新周期（默认 10 分钟，≥60）
	RefreshIntervalSeconds int `json:",default=600"`
	// IngestIntervalSeconds 做同款摄入周期（默认 15 秒，≥5）
	IngestIntervalSeconds int `json:",default=15"`
	// IngestLagSeconds 摄入滞后窗口（避开未提交 / 慢提交的轮次）
	IngestLagSeconds int `json:",default=5"`
	// IngestBatch 每轮最多摄入多少行（一轮内循环读到追平或读满 10 批）
	IngestBatch int `json:",default=500"`
	// FirstRefreshDelaySeconds 选主后多久出第一版榜单
	FirstRefreshDelaySeconds int `json:",default=30"`
	// LatestCacheTTLSeconds 每个范围最新榜单 Redis 缓存的安全 TTL
	LatestCacheTTLSeconds int `json:",default=3600"`
	// DefaultParams 评分参数的 yaml 默认值（JSON 对象，键同 §14.5.7，如 {"top-n":10}）；管理端保存的参数优先
	DefaultParams string `json:",optional"`
	// Redis 单独的计数 Redis（可选）；不配则复用 Auth.Session 的 Redis，都没有则降级为 DB 计数 + 进程内锁（仅开发）
	Redis struct {
		Addr     string `json:",optional"`
		Password string `json:",optional"`
		DB       int    `json:",optional"`
	}
}

func (c Conf) refreshInterval() time.Duration {
	if c.RefreshIntervalSeconds < 60 {
		return 10 * time.Minute
	}
	return time.Duration(c.RefreshIntervalSeconds) * time.Second
}

func (c Conf) ingestInterval() time.Duration {
	if c.IngestIntervalSeconds < 5 {
		return 15 * time.Second
	}
	return time.Duration(c.IngestIntervalSeconds) * time.Second
}

func (c Conf) latestTTL() time.Duration {
	if c.LatestCacheTTLSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(c.LatestCacheTTLSeconds) * time.Second
}

// Defaults yaml 默认参数（非法时回退内置默认并告警）。
func (c Conf) Defaults() algo.Params {
	p := algo.DefaultParams()
	if c.DefaultParams == "" {
		return p
	}
	q := p
	if err := json.Unmarshal([]byte(c.DefaultParams), &q); err != nil {
		logx.Errorf("plaza headline: DefaultParams 非法，用内置默认: %v", err)
		return p
	}
	if errs := (algo.Config{Global: q}).Validate(nil); len(errs) > 0 {
		logx.Errorf("plaza headline: DefaultParams 越界，用内置默认: %v", errs)
		return p
	}
	return q
}

// Service 头条服务。
type Service struct {
	repo    *store.Repo
	sig     *signals.Store
	catalog CatalogSource
	conf    Conf
	now     func() time.Time

	catMu       sync.Mutex
	cat         *store.Catalog
	catVer      string
	catChecked  time.Time
	refreshMu   sync.Mutex // 无 Redis 时的进程内刷新互斥
	ctlCacheMu  sync.Mutex
	rulesCache  *ruleCache
	triggerOnce sync.Map // 网关「新范围无榜」异步触发的去重（scope → 时刻）
}

type ruleCache struct {
	at     time.Time
	rules  []store.HeadlineRule
	blocks []store.Block
}

// New 组装服务。catalog 为 nil 时用 repo 直读。
func New(repo *store.Repo, sig *signals.Store, catalog CatalogSource, conf Conf) *Service {
	if catalog == nil {
		catalog = repo
	}
	return &Service{repo: repo, sig: sig, catalog: catalog, conf: conf, now: time.Now}
}

// WithClock 注入时钟（单测）。
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Repo 数据访问（handler 层分页等直接用）。
func (s *Service) Repo() *store.Repo { return s.repo }

func errNotReady() error { return xerr.NewReason(xerr.NotReady, ReasonNotReady, "功能暂未开放") }

func (s *Service) ready() error {
	if s == nil || s.repo == nil || s.repo.DB == nil {
		return errNotReady()
	}
	return nil
}

// Catalog 目录（进程内缓存：Redis 目录版本号变了或超过 10 分钟就重载；版本号每 2 秒最多比对一次）。
func (s *Service) Catalog(ctx context.Context) (*store.Catalog, error) {
	now := s.now()
	s.catMu.Lock()
	defer s.catMu.Unlock()
	stale := s.cat == nil || now.Sub(s.cat.LoadedAt) > 10*time.Minute
	if !stale && now.Sub(s.catChecked) >= 2*time.Second {
		s.catChecked = now
		if v, err := s.sig.CatalogVersion(ctx); err == nil && v != s.catVer {
			stale = true
			s.catVer = v
		}
	}
	if !stale {
		return s.cat, nil
	}
	cat, err := s.catalog.LoadCatalog(ctx, now)
	if err != nil {
		if s.cat != nil {
			logx.WithContext(ctx).Errorf("plaza headline: 重载目录失败，沿用旧目录: %v", err)
			return s.cat, nil
		}
		return nil, err
	}
	if v, err := s.sig.CatalogVersion(ctx); err == nil {
		s.catVer = v
	}
	s.cat, s.catChecked = cat, now
	return cat, nil
}

// InvalidateCatalog 让下次读取重载目录（本进程写了规则 / 模板后调用）。
func (s *Service) InvalidateCatalog() {
	s.catMu.Lock()
	s.cat = nil
	s.catMu.Unlock()
}

// LoadConfig 生效参数：控制行 params_json 优先，缺失项回退 yaml 默认；坏参数告警并整体回退。
func (s *Service) LoadConfig(ctx context.Context) (algo.Config, *store.Ctl, error) {
	ctl, err := s.repo.Ctl(ctx)
	if err != nil {
		return algo.Config{}, nil, err
	}
	cfg, warn := algo.ParseConfig(ctl.ParamsJSON, s.conf.Defaults())
	if warn != nil {
		logx.WithContext(ctx).Errorf("plaza headline: %v", warn)
	}
	if keys := algo.UnknownWeightKeys(ctl.ParamsJSON); len(keys) > 0 {
		logx.WithContext(ctx).Infof("plaza headline: 参数里有未实现的信号 %v，已忽略", keys)
	}
	return cfg, ctl, nil
}

// rulesAndBlocks 生效的人工规则与抑制（请求路径 5 秒缓存；刷新路径 fresh=true 直读）。
func (s *Service) rulesAndBlocks(ctx context.Context, fresh bool) ([]store.HeadlineRule, []store.Block, error) {
	now := s.now()
	s.ctlCacheMu.Lock()
	c := s.rulesCache
	s.ctlCacheMu.Unlock()
	if !fresh && c != nil && now.Sub(c.at) < 5*time.Second {
		return c.rules, c.blocks, nil
	}
	rules, err := s.repo.ActiveRules(ctx, now)
	if err != nil {
		return nil, nil, err
	}
	blocks, err := s.repo.ActiveBlocks(ctx, now)
	if err != nil {
		return nil, nil, err
	}
	s.ctlCacheMu.Lock()
	s.rulesCache = &ruleCache{at: now, rules: rules, blocks: blocks}
	s.ctlCacheMu.Unlock()
	return rules, blocks, nil
}

func (s *Service) invalidateRules() {
	s.ctlCacheMu.Lock()
	s.rulesCache = nil
	s.ctlCacheMu.Unlock()
}

// readSignals 所有范围共享一次信号读取：Redis 优先；Redis 抛错 → DB 聚合；Redis 近 7 天全空 → DB（新环境 / 被清空，
// 由后台任务随后校准补齐）。
func (s *Service) readSignals(ctx context.Context, now time.Time, p algo.Params) (map[int64]algo.Signal, string, error) {
	if s.sig.Enabled() {
		has, err := s.sig.HasAnyBucket(ctx, now)
		if err == nil && has {
			sigs, err := s.sig.ReadSignals(ctx, now)
			if err == nil {
				return sigs, SourceRedis, nil
			}
			logx.WithContext(ctx).Errorf("plaza headline: 读 Redis 信号失败，降级 DB: %v", err)
		} else if err != nil {
			logx.WithContext(ctx).Errorf("plaza headline: Redis 不可用，降级 DB: %v", err)
		}
	}
	sigs, err := s.dbSignals(ctx, now, p)
	return sigs, SourceDB, err
}

// signalWindowStart 近 14 天信号窗口的读取起点：14×24 小时前所在 CST 自然日的 0 点（≤ 窗口起点，最多多读 23 小时）。
func signalWindowStart(now time.Time) time.Time {
	d := now.In(algo.CST).Add(-time.Duration(2*algo.HoursPerWeek) * time.Hour)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, algo.CST)
}

// dbSignals DB 聚合（与 Lua 摄入同口径：algo.CountTurns）：近 14 天。
func (s *Service) dbSignals(ctx context.Context, now time.Time, p algo.Params) (map[int64]algo.Signal, error) {
	// 读取起点对齐到 CST 日界：每日封顶按 CST 自然日计，窗口从半天开始会漏掉当天更早的尝试、让首日超封顶（L10）
	turns, err := s.repo.TurnsBetween(ctx, signalWindowStart(now), now, 0)
	if err != nil {
		return nil, err
	}
	return algo.CountTurns(turns, p.PerUserDailyCap, p.RapidDedupeSeconds).Signals(now), nil
}
