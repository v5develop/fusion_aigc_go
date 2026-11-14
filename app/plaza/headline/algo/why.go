package algo

import (
	"fmt"
	"strconv"
	"strings"
)

// WhyFill 冷启动补位条目的固定推荐语（无数字）。
const WhyFill = "新上架"

// FormatCount 推荐语里的次数：<1000 原样，≥1000 写 1.2k，≥10000 写 1.2w（一位小数，去掉 .0）。
func FormatCount(n int) string {
	trim := func(v float64, unit string) string {
		s := strconv.FormatFloat(float64(int(v*10))/10, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + unit
	}
	switch {
	case n >= 10000:
		return trim(float64(n)/10000, "w")
	case n >= 1000:
		return trim(float64(n)/1000, "k")
	default:
		return strconv.Itoa(n)
	}
}

// UsageVisible 近 7 天次数是否达到展示门槛（不足时用户端不展示数字，只给「热」标签和名次）。
func UsageVisible(uses7d int, p Params) bool { return uses7d >= p.DisplayMinUses }

// AutoWhy 算法条目的推荐语（14.5.6，按顺序命中第一条；≤60 字，只来自此表，不拼标题与用户内容）。
// 次数不足展示门槛时用不带数字的同义文案，避免「近 7 天 3 次复刻」这种暴露冷清的说法。
func AutoWhy(st Stats, p Params) string {
	fresh := p.FreshDays > 0 && st.AgeDays <= p.FreshDays
	trend := st.Trend()
	if !UsageVisible(st.Uses7d, p) {
		switch {
		case fresh && st.PrevUses == 0:
			return "新上架 · 近期热门"
		case st.Velocity >= 0.5:
			return "今日热度上升"
		default:
			return "近 7 天热门"
		}
	}
	n := FormatCount(st.Uses7d)
	switch {
	case fresh && st.PrevUses == 0:
		return fmt.Sprintf("新上架 · 近 7 天 %s 次复刻", n)
	case st.Velocity >= 0.5:
		return fmt.Sprintf("今日热度上升 · 近 7 天 %s 次复刻", n)
	case trend != nil && *trend >= 20:
		return fmt.Sprintf("近 7 天复刻 %s 次，较前 7 天 +%d%%", n, *trend)
	case st.PrevUses == 0:
		return fmt.Sprintf("近 7 天复刻 %s 次，新上榜", n)
	default:
		return fmt.Sprintf("近 7 天复刻 %s 次", n)
	}
}
