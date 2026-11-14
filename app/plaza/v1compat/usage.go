package v1compat

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 复刻 Java PlazaTemplateUsageStatsService：滚动窗口 [now-7d, now) 与 [now-14d, now-7d)，
// 实例内缓存 10 分钟；刷新失败沿用上一份（并把计算时刻推后一个 TTL），没有上一份就当全 0（不缓存）。
// 列表的 usageCount 与头条的 uses / previousUses / trend / isNew 都来自它；两边缓存相位不同，diff 对这些字段做容差。

var shanghai = mustLoc("Asia/Shanghai")

func mustLoc(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600) // 无 tzdata 时的兜底（Asia/Shanghai 无夏令时）
}

const (
	usageTTL    = 10 * time.Minute
	usageWindow = 7 * 24 * time.Hour
)

// usage 一个模板的统计（PlazaTemplateUsage）。
type usage struct{ uses, previous int64 }

// trend (uses − previous) / max(previous, 1) 的取整百分比（Math.round）；previous=0 → nil。
func (u usage) trend() *int64 {
	if u.previous == 0 {
		return nil
	}
	v := int64(math.Floor(float64(u.uses-u.previous)*100.0/float64(max(u.previous, 1)) + 0.5))
	return &v
}

type usageSnapshot struct {
	windowStart, windowEnd, computedAt time.Time // Asia/Shanghai 本地时间
	byID                               map[int64]usage
}

func (s *usageSnapshot) of(id int64) usage {
	if s == nil {
		return usage{}
	}
	return s.byID[id]
}

func (s *usageSnapshot) startDate() string { return s.windowStart.Format("2006-01-02") }
func (s *usageSnapshot) endDate() string   { return s.windowEnd.Format("2006-01-02") }

type usageStats struct {
	src Source
	now func() time.Time

	mu     sync.Mutex
	cached *usageSnapshot
}

func newUsageStats(src Source, now func() time.Time) *usageStats {
	if now == nil {
		now = time.Now
	}
	return &usageStats{src: src, now: now}
}

func (u *usageStats) snapshot(ctx context.Context) *usageSnapshot {
	now := u.now().In(shanghai)
	u.mu.Lock()
	defer u.mu.Unlock()
	if c := u.cached; c != nil && c.computedAt.Add(usageTTL).After(now) {
		return c
	}
	end := now.Truncate(time.Second)
	mid := end.Add(-usageWindow)
	start := mid.Add(-usageWindow)
	rows, err := u.src.CountUses(ctx, start, mid, end)
	if err != nil {
		prev := u.cached
		logx.WithContext(ctx).Errorf("plaza v1compat: 复刻统计刷新失败 判据=聚合查询异常 沿用=%s 原因=%v",
			map[bool]string{true: "空统计", false: "上一份快照"}[prev == nil], err)
		if prev != nil {
			stale := &usageSnapshot{windowStart: prev.windowStart, windowEnd: prev.windowEnd, computedAt: now, byID: prev.byID}
			u.cached = stale
			return stale
		}
		return &usageSnapshot{windowStart: end.Add(-usageWindow), windowEnd: end, computedAt: now, byID: map[int64]usage{}}
	}
	by := make(map[int64]usage, len(rows))
	for _, r := range rows {
		by[r.TemplateID] = usage{uses: max(r.Uses, 0), previous: max(r.PreviousUses, 0)}
	}
	fresh := &usageSnapshot{windowStart: mid, windowEnd: end, computedAt: now, byID: by}
	u.cached = fresh
	return fresh
}
