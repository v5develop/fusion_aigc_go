package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件：方案卡 ETA（DESIGN §5.17）的历史校准。
//
// 口径：一「轮」= 同时在途 ≤ MaxInFlight 段，R = ceil(N / MaxInFlight) 轮，区间 = R × 每轮 [P50, P90]。
// 每轮耗时的样本 = 历史成功 job（status=2）里每一段「第一次开始生成 → 成功出片」的耗时（含自动重试，不含用户重做、
// 不含排队等空位与待继续的等待），由 Store 侧查询现算（repo.EtaSamples），不另落表。
// 分桶：模型组 × 段数桶（1 / 2 / 3 / 4+）× 单段生成时长桶（0-5 / 6-10 / 11-15 / 16+ 秒，方案取各段最长的那段）。
// 精确桶样本 ≥ MinSamples 用精确桶；否则用同模型组同时长桶、不分段数的粗桶；仍不足回落先验（Config.EtaRoundP50/P90），prior=true。
// 统计在进程内缓存，过期后后台异步重算（请求从不等库）；第一次查询时还没有统计，回落先验。

// EtaCalibration 校准口径。零值 = 默认。
type EtaCalibration struct {
	Window     time.Duration // 统计窗口（默认 30 天，按 job 完成时间）
	MinSamples int           // 每桶最少样本（默认 20）
	MaxRows    int           // 一次最多取多少段样本（默认 5000，最近完成的优先）
	Refresh    time.Duration // 统计缓存多久重算一次（默认 10 分钟）
	Timeout    time.Duration // 一次重算的查库时限（默认 10 秒）
}

func (c EtaCalibration) withDefaults() EtaCalibration {
	if c.Window <= 0 {
		c.Window = 30 * 24 * time.Hour
	}
	if c.MinSamples <= 0 {
		c.MinSamples = 20
	}
	if c.MaxRows <= 0 {
		c.MaxRows = 5000
	}
	if c.Refresh <= 0 {
		c.Refresh = 10 * time.Minute
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	return c
}

// EtaSample 一段的历史耗时样本。
type EtaSample struct {
	ModelGroup string  // 出片模型组（job.input.modelGroup）
	SegTotal   int     // 该 job 的段数
	GenSeconds int     // 该段生成时长（计费秒数）
	Seconds    float64 // 第一次开始生成 → 成功出片的耗时（秒）
}

// EtaSampleSource 历史样本（生产实现 repo.Repo.EtaSamples）：since 之后完成的成功 job，最多 limit 段，最近的优先。
type EtaSampleSource interface {
	EtaSamples(ctx context.Context, since time.Time, limit int) ([]EtaSample, error)
}

// 分桶。
const etaAnySegs = "*"

// EtaSegBucket 段数桶。
func EtaSegBucket(n int) string {
	switch {
	case n <= 1:
		return "1"
	case n == 2:
		return "2"
	case n == 3:
		return "3"
	}
	return "4+"
}

// EtaDurBucket 单段生成时长桶（秒）。
func EtaDurBucket(sec int) string {
	switch {
	case sec <= 5:
		return "0-5"
	case sec <= 10:
		return "6-10"
	case sec <= 15:
		return "11-15"
	}
	return "16+"
}

type etaKey struct{ group, segs, dur string }

// etaStat 一个桶的统计（秒）。
type etaStat struct {
	P50, P90 float64
	N        int
}

// buildEtaTable 样本 → 精确桶与粗桶（segs="*"）的 P50 / P90（最近秩法）。
func buildEtaTable(samples []EtaSample) map[etaKey]etaStat {
	vals := map[etaKey][]float64{}
	for _, x := range samples {
		if x.ModelGroup == "" || x.Seconds <= 0 || math.IsNaN(x.Seconds) || math.IsInf(x.Seconds, 0) {
			continue
		}
		dur := EtaDurBucket(x.GenSeconds)
		exact := etaKey{x.ModelGroup, EtaSegBucket(x.SegTotal), dur}
		coarse := etaKey{x.ModelGroup, etaAnySegs, dur}
		vals[exact] = append(vals[exact], x.Seconds)
		vals[coarse] = append(vals[coarse], x.Seconds)
	}
	out := make(map[etaKey]etaStat, len(vals))
	for k, v := range vals {
		sort.Float64s(v)
		out[k] = etaStat{P50: percentile(v, 0.5), P90: percentile(v, 0.9), N: len(v)}
	}
	return out
}

// percentile 最近秩法（sorted 已升序、非空）。
func percentile(sorted []float64, q float64) float64 {
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// EtaCalibrator 历史校准（进程内一份，跨运行参数重建的 Service 共用：WithEtaCalibrator）。
type EtaCalibrator struct {
	src EtaSampleSource
	cfg EtaCalibration
	now func() time.Time

	mu      sync.RWMutex
	table   map[etaKey]etaStat
	at      time.Time // 上次成功重算的时刻（零值 = 还没有统计）
	tried   time.Time // 上次发起重算的时刻（失败也记，避免失败后每个请求都打库）
	loading atomic.Bool
}

// NewEtaCalibrator 装配。src 为 nil 时永远回落先验。
func NewEtaCalibrator(src EtaSampleSource, cfg EtaCalibration) *EtaCalibrator {
	return &EtaCalibrator{src: src, cfg: cfg.withDefaults(), now: time.Now}
}

// Refresh 同步重算一次（启动预热 / 测试）。失败保留上一次的统计。
func (c *EtaCalibrator) Refresh(ctx context.Context) error {
	if c == nil || c.src == nil {
		return nil
	}
	now := c.now()
	c.mu.Lock()
	c.tried = now
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	samples, err := c.src.EtaSamples(ctx, now.Add(-c.cfg.Window), c.cfg.MaxRows)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: ETA 历史统计重算失败，沿用上一次 判据=ETA_STATS_FAILED: %v", err)
		return err
	}
	tbl := buildEtaTable(samples)
	c.mu.Lock()
	c.table, c.at = tbl, now
	c.mu.Unlock()
	logx.WithContext(ctx).Infof("replicate: ETA 历史统计已重算 样本=%d 桶=%d", len(samples), len(tbl))
	return nil
}

// maybeRefresh 统计过期（或从没有）且上次发起已超过刷新间隔时，后台异步重算一次（单飞，脱离请求上下文）。
func (c *EtaCalibrator) maybeRefresh() {
	if c.src == nil {
		return
	}
	c.mu.RLock()
	stale := c.now().Sub(c.tried) >= c.cfg.Refresh
	c.mu.RUnlock()
	if !stale || !c.loading.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.loading.Store(false)
		_ = c.Refresh(context.Background())
	}()
}

// lookup 每轮耗时（秒）：精确桶 → 粗桶 → 没有（回落先验）。
func (c *EtaCalibrator) lookup(group string, segTotal, maxGenSec int) (etaStat, bool) {
	if c == nil {
		return etaStat{}, false
	}
	c.maybeRefresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	dur := EtaDurBucket(maxGenSec)
	for _, k := range []etaKey{{group, EtaSegBucket(segTotal), dur}, {group, etaAnySegs, dur}} {
		if st, ok := c.table[k]; ok && st.N >= c.cfg.MinSamples && st.P50 > 0 {
			return st, true
		}
	}
	return etaStat{}, false
}

// eta 方案卡的预计耗时：R = ceil(N / MaxInFlight) 轮 × 每轮 [P50, P90]。整张方案用同一个桶（模型组 × 段数桶 × 最长段时长桶），
// 所以 first / rest / total 要么全部来自历史（prior=false），要么全部是先验（prior=true）。
func (s *Service) eta(group string, segs []PlanSegmentView) ETAView {
	n := len(segs)
	p50, p90 := s.cfg.EtaRoundP50.Seconds(), s.cfg.EtaRoundP90.Seconds()
	prior := true
	if s.etaCal != nil && n > 0 {
		maxGen := 0
		for _, sg := range segs {
			maxGen = max(maxGen, sg.GenDuration)
		}
		if st, ok := s.etaCal.lookup(group, n, maxGen); ok {
			p50, p90, prior = st.P50, math.Max(st.P90, st.P50), false
		}
	}
	r := func(k int) *ETARange {
		rounds := (k + s.cfg.MaxInFlight - 1) / s.cfg.MaxInFlight
		lo, hi := int(math.Round(p50))*rounds, int(math.Round(p90))*rounds
		if hi < lo {
			hi = lo
		}
		return &ETARange{LowSec: lo, HighSec: hi, Text: etaText(lo, hi)}
	}
	v := ETAView{Total: *r(n), Prior: prior}
	if n > 1 {
		v.First, v.Rest = r(1), r(n-1)
	}
	return v
}

// etaText 文案以分钟计（向上取整）：上下限相同只写一个数。
func etaText(lo, hi int) string {
	a, b := (lo+59)/60, (hi+59)/60
	if a == b {
		return fmt.Sprintf("预计 %d 分钟", b)
	}
	return fmt.Sprintf("预计 %d–%d 分钟", a, b)
}
