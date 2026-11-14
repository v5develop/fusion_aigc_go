package algo

import (
	"math"
	"time"
)

// CST 与 Java 一致的库时区（Asia/Shanghai，无夏令时）。小时桶、自然日封顶、「今天」都按它取。
var CST = time.FixedZone("CST", 8*3600)

// HoursPerWeek 一个统计窗口（滚动 7 天）的小时桶数。
const HoursPerWeek = 7 * 24

// HourKey 小时桶键：该小时起点的 unix 秒（按 CST 对齐到整点；CST 无夏令时，所以与 UTC 整点一致）。
func HourKey(t time.Time) int64 { return t.Truncate(time.Hour).Unix() }

// HourLabel Redis 键用的 yyyyMMddHH（CST）。
func HourLabel(t time.Time) string { return t.In(CST).Format("2006010215") }

// DayLabel Redis 键用的 yyyyMMdd（CST）。
func DayLabel(t time.Time) string { return t.In(CST).Format("20060102") }

// Signal 一个模板的原始信号：已封顶、去重的小时计数 + 近 7 天去重用户数。
type Signal struct {
	TemplateID int64
	// Hours 小时桶：键 HourKey，值为该小时计入的做同款次数。可含 14 天以内的桶（再往前 7 天用于 prevUses / trend）。
	Hours       map[int64]int
	UniqueUsers int
}

// Stats 一个模板的统计量（14.5.1），所有范围共享。
type Stats struct {
	Uses7d      int     `json:"uses7d"`
	PrevUses    int     `json:"prevUses7d"`
	R24         int     `json:"r24"`
	Rate6d      float64 `json:"rate6d"`
	Decayed     float64 `json:"decayed"`
	Velocity    float64 `json:"velocity"`
	Freshness   float64 `json:"freshness"`
	AgeDays     int     `json:"ageDays"`
	UniqueUsers int     `json:"uniqueUsers"`
}

// Trend 较前 7 天的百分比（与 Java PlazaTemplateUsage.trendPercent 同口径：四舍五入、前 7 天为 0 时 nil）。
func (s Stats) Trend() *int {
	if s.PrevUses <= 0 {
		return nil
	}
	v := int(math.Floor(float64(s.Uses7d-s.PrevUses)*100/float64(s.PrevUses) + 0.5))
	return &v
}

// IsNew 前 7 天没人用过（Java isNew 同口径）。
func (s Stats) IsNew() bool { return s.PrevUses == 0 }

// AgeDays 今天（CST）与模板创建日（CST）之间的整天数，最小 0。
func AgeDays(createdAt, now time.Time) int {
	if createdAt.IsZero() {
		return math.MaxInt32
	}
	cy, cm, cd := createdAt.In(CST).Date()
	ny, nm, nd := now.In(CST).Date()
	d := int(time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC).Sub(time.Date(cy, cm, cd, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// Freshness max(0, 1 − ageDays/freshDays)；freshDays=0 时恒为 0。
func Freshness(ageDays, freshDays int) float64 {
	if freshDays <= 0 {
		return 0
	}
	return math.Max(0, 1-float64(ageDays)/float64(freshDays))
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// ComputeStats 由小时桶算出统计量。
//
//	窗口：当前小时（含）往前 168 个桶为「近 7 天」，再往前 168 个为「前 7 天」；最近 24 个桶为 r24。
//	衰减：每个桶按「now − 桶中点」的小时数衰减，半衰期 halfLifeHours；桶中点晚于 now（当前小时前半段）按 0 计。
//	增速：rate6d = (uses7d − r24) / 6；velocity = clamp((r24 − rate6d) / max(rate6d, minBase), min, max)。
func ComputeStats(sig Signal, createdAt, now time.Time, p Params) Stats {
	cur := HourKey(now)
	weekStart := cur - int64(HoursPerWeek-1)*3600
	prevStart := weekStart - int64(HoursPerWeek)*3600
	dayStart := cur - 23*3600
	var st Stats
	nowSec := float64(now.UnixNano()) / 1e9
	for h, c := range sig.Hours {
		if c <= 0 || h > cur {
			continue
		}
		switch {
		case h >= weekStart:
			st.Uses7d += c
			if h >= dayStart {
				st.R24 += c
			}
			age := math.Max(0, (nowSec-float64(h+1800))/3600)
			hl := p.HalfLifeHours
			if hl <= 0 {
				hl = 24
			}
			st.Decayed += float64(c) * math.Pow(0.5, age/hl)
		case h >= prevStart:
			st.PrevUses += c
		}
	}
	st.Rate6d = float64(st.Uses7d-st.R24) / 6
	base := math.Max(st.Rate6d, p.Velocity.MinBase)
	if base <= 0 {
		base = 1
	}
	st.Velocity = clamp((float64(st.R24)-st.Rate6d)/base, p.Velocity.Min, p.Velocity.Max)
	st.AgeDays = AgeDays(createdAt, now)
	st.Freshness = Freshness(st.AgeDays, p.FreshDays)
	st.UniqueUsers = sig.UniqueUsers
	st.Decayed = round4(st.Decayed)
	st.Rate6d = round4(st.Rate6d)
	st.Velocity = round4(st.Velocity)
	st.Freshness = round4(st.Freshness)
	return st
}

// round4 四位小数（与快照 DECIMAL(10,4) 一致，避免浮点尾差让「同输入同输出」在序列化后失真）。
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }
