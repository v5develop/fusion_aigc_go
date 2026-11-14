package algo

import (
	"errors"
	"sort"
	"time"
)

// 取消推荐的时长快捷项（14.9 / 产品方案 3.4：24 小时 / 到本周日 / 自定义 / 永久）。
const (
	Duration24h       = "24h"
	DurationSunday    = "sunday"
	DurationCustom    = "custom"
	DurationPermanent = "permanent"
)

// 抑制种类。
const (
	BlockCancel = "CANCEL" // 取消到某时刻
	BlockBlock  = "BLOCK"  // 永久屏蔽
)

// ErrUntilNotFuture 自定义截止不晚于现在。
var ErrUntilNotFuture = errors.New("截止时间必须晚于现在")

// ErrUnknownDuration 未知的时长快捷项。
var ErrUnknownDuration = errors.New("未知的取消时长")

// SuppressUntil 由时长快捷项算出抑制的种类与截止时刻（CST）：
//   - 24h：now + 24 小时；
//   - sunday：本周日 24:00（即下周一 00:00）；今天就是周日则为今晚 24:00；
//   - custom：customDate 当日 24:00（customDate 只取日期部分），必须晚于 now；
//   - permanent：永久屏蔽（until = nil）。
func SuppressUntil(duration string, customDate time.Time, now time.Time) (kind string, until *time.Time, err error) {
	switch duration {
	case Duration24h:
		t := now.Add(24 * time.Hour)
		return BlockCancel, &t, nil
	case DurationSunday:
		today := dateOf(now)
		daysToMonday := (8 - int(today.Weekday())) % 7
		if daysToMonday == 0 {
			daysToMonday = 7
		}
		if today.Weekday() == time.Sunday {
			daysToMonday = 1
		}
		t := today.AddDate(0, 0, daysToMonday)
		return BlockCancel, &t, nil
	case DurationCustom:
		if customDate.IsZero() {
			return "", nil, ErrUntilNotFuture
		}
		t := dateOf(customDate).AddDate(0, 0, 1)
		if !t.After(now) {
			return "", nil, ErrUntilNotFuture
		}
		return BlockCancel, &t, nil
	case DurationPermanent:
		return BlockBlock, nil, nil
	}
	return "", nil, ErrUnknownDuration
}

// SnapshotMeta 保留清理用的版本元信息。
type SnapshotMeta struct {
	Version    int64
	ComputedAt time.Time
}

// ExpiredSnapshots 保留策略（14.6）：最近 24 小时每版保留；24 小时 ~ retentionDays 只保留每个小时的第一版；
// 超期整版删除；无论如何至少保留最近 keepLatest 个版本。返回要删除的版本号（升序）。
func ExpiredSnapshots(metas []SnapshotMeta, now time.Time, retentionDays, keepLatest int) []int64 {
	sorted := append([]SnapshotMeta(nil), metas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	protected := map[int64]bool{}
	for i := len(sorted) - 1; i >= 0 && len(sorted)-i <= keepLatest; i-- {
		protected[sorted[i].Version] = true
	}
	dayAgo := now.Add(-24 * time.Hour)
	limit := now.AddDate(0, 0, -retentionDays)
	firstOfHour := map[int64]bool{}
	var out []int64
	for _, m := range sorted {
		hour := HourKey(m.ComputedAt)
		keep := false
		switch {
		case !m.ComputedAt.Before(dayAgo):
			keep = true
		case !m.ComputedAt.Before(limit):
			if !firstOfHour[hour] {
				firstOfHour[hour] = true
				keep = true
			}
		}
		if !keep && !protected[m.Version] {
			out = append(out, m.Version)
		}
	}
	return out
}

// Turn 一次做同款尝试（afv_agent_turn 一行的投影）。
type Turn struct {
	RowID      int64
	UserID     int64
	TemplateID int64
	CreatedAt  time.Time
}

// TurnCounts 由原始尝试得到的计数：小时桶 + 每日去重用户集合。
type TurnCounts struct {
	// Hours 模板 → 小时桶 → 计入数
	Hours map[int64]map[int64]int
	// DayUsers 模板 → 日（yyyyMMdd）→ 计入过的用户集合
	DayUsers map[int64]map[string]map[int64]bool
}

// CountTurns 防刷计数（14.4.3），与 Redis Lua 摄入同语义，DB 降级 / 每小时校准都用它：
//   - 同一用户对同一模板，距上一次「尝试」（不论是否计入）不足 rapidSeconds 秒的视为重复点击，不计，且刷新这一窗口；
//   - 每个 CST 自然日同一用户对同一模板最多计 dailyCap 次。
//
// 输入顺序无要求（内部按 用户、模板、时间、行 id 排序），重复的行 id 只算一次。
func CountTurns(turns []Turn, dailyCap, rapidSeconds int) TurnCounts {
	sorted := append([]Turn(nil), turns...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.TemplateID != b.TemplateID {
			return a.TemplateID < b.TemplateID
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.RowID < b.RowID
	})
	out := TurnCounts{Hours: map[int64]map[int64]int{}, DayUsers: map[int64]map[string]map[int64]bool{}}
	type key struct{ tpl, user int64 }
	last := map[key]time.Time{}
	daily := map[key]map[string]int{}
	seen := map[int64]bool{}
	rapid := time.Duration(rapidSeconds) * time.Second
	for _, t := range sorted {
		if t.TemplateID <= 0 || seen[t.RowID] {
			continue
		}
		seen[t.RowID] = true
		k := key{t.TemplateID, t.UserID}
		prev, had := last[k]
		last[k] = t.CreatedAt
		if had && rapid > 0 && t.CreatedAt.Sub(prev) < rapid {
			continue
		}
		day := DayLabel(t.CreatedAt)
		if daily[k] == nil {
			daily[k] = map[string]int{}
		}
		daily[k][day]++
		if daily[k][day] > dailyCap {
			continue
		}
		if out.Hours[t.TemplateID] == nil {
			out.Hours[t.TemplateID] = map[int64]int{}
		}
		out.Hours[t.TemplateID][HourKey(t.CreatedAt)]++
		if out.DayUsers[t.TemplateID] == nil {
			out.DayUsers[t.TemplateID] = map[string]map[int64]bool{}
		}
		if out.DayUsers[t.TemplateID][day] == nil {
			out.DayUsers[t.TemplateID][day] = map[int64]bool{}
		}
		out.DayUsers[t.TemplateID][day][t.UserID] = true
	}
	return out
}

// Signals 由计数得到打分信号：小时桶原样，去重用户 = 近 7 天（含今天的 7 个自然日）出现过的用户并集。
func (tc TurnCounts) Signals(now time.Time) map[int64]Signal {
	days := map[string]bool{}
	for i := 0; i < 7; i++ {
		days[DayLabel(now.AddDate(0, 0, -i))] = true
	}
	out := make(map[int64]Signal, len(tc.Hours))
	for tpl, hours := range tc.Hours {
		users := map[int64]bool{}
		for day, set := range tc.DayUsers[tpl] {
			if !days[day] {
				continue
			}
			for u := range set {
				users[u] = true
			}
		}
		out[tpl] = Signal{TemplateID: tpl, Hours: hours, UniqueUsers: len(users)}
	}
	return out
}
