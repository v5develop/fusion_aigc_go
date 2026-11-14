// Package quote 分段报价汇总（纯函数）。单价不在这里算：每段点数由 pkg/billing 的 Quote 按秒给出，
// 这里只负责「每段 → 先做第 1 段 / 其余段 / 合计」的口径，保证展示价 = Σ 段价 = 冻结价（DESIGN §9.2、§9.5）。
package quote

import "fmt"

// Stage 确认阶段。
const (
	StageFirst = "first" // 先做第 1 段
	StageAll   = "all"   // 一次做完全部段
	StageRest  = "rest"  // 第 1 段之后继续其余段
)

// Line 一段的报价。
type Line struct {
	Index   int   `json:"index"`
	Seconds int   `json:"seconds"` // 计费秒数 = 生成时长（含拼接处多生成的部分）
	Points  int64 `json:"points"`
}

// Stages 分阶段报价。
type Stages struct {
	Lines        []Line `json:"lines"`
	First        int64  `json:"first"`        // 第 1 段
	Rest         int64  `json:"rest"`         // 第 2..N 段之和
	Total        int64  `json:"total"`        // 全部
	RestSegments int    `json:"restSegments"` // 其余段数
	TotalSeconds int    `json:"totalSeconds"`
}

// Summarize 汇总；lines 必须按段号 1..N 连续，否则报错（防止漏段少收或多收）。
func Summarize(lines []Line) (Stages, error) {
	var s Stages
	for i, l := range lines {
		if l.Index != i+1 {
			return Stages{}, fmt.Errorf("quote: 段号不连续：第 %d 行是第 %d 段", i+1, l.Index)
		}
		if l.Points < 0 || l.Seconds <= 0 {
			return Stages{}, fmt.Errorf("quote: 第 %d 段报价非法（%d 秒 %d 点）", l.Index, l.Seconds, l.Points)
		}
		s.Total += l.Points
		s.TotalSeconds += l.Seconds
		if i == 0 {
			s.First = l.Points
		} else {
			s.Rest += l.Points
			s.RestSegments++
		}
	}
	s.Lines = append([]Line(nil), lines...)
	return s, nil
}

// Indexes 某阶段要预留的段号。
func Indexes(stage string, n int) []int {
	var out []int
	switch stage {
	case StageFirst:
		if n >= 1 {
			out = []int{1}
		}
	case StageRest:
		for i := 2; i <= n; i++ {
			out = append(out, i)
		}
	case StageAll:
		for i := 1; i <= n; i++ {
			out = append(out, i)
		}
	}
	return out
}

// PointsOf 某阶段的合计点数。
func (s Stages) PointsOf(stage string) int64 {
	switch stage {
	case StageFirst:
		return s.First
	case StageRest:
		return s.Rest
	}
	return s.Total
}

// DefaultStage 默认确认方式（EXPERIENCE 0.1 #1）：多段默认先做第 1 段；用户勾过「以后直接全部做」则全部；单段只有 all。
func DefaultStage(segments int, autoContinue bool) string {
	if segments > 1 && !autoContinue {
		return StageFirst
	}
	return StageAll
}
