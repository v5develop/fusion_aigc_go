// Package segment 视频做同款的分段规划（纯函数：无 IO、无时钟、无随机，同输入同输出）。
//
// 口径（DESIGN §5.9、§5.17，EXPERIENCE 0.1 #4）：
//   - 每段生成窗口 gen_duration ≤ SegCap（默认 15 秒，网关 duration_s 为整数，向上取整后夹到 [SpecMin, SegCap]）；
//   - 段与段之间多生成 Overlap（默认 0.4 秒）让拼接处更顺：第 k>1 段的生成窗口从 t0-Overlap 开始，
//     所以多段时每段时间轴长度上限 segTarget = SegCap - Overlap；只有一段时没有拼接，整条 ≤ SegCap 即可；
//   - 切点优先级：镜头切换（cut）> 说话停顿（pause）> 按时长均分（even）。
//     v4 的「视觉最静帧」依赖媒体服务新算子 /frames/pick，尚不存在，这里不伪造，降级为均分；
//   - 带可行性约束的贪心：每次选切点的下界取 max(s+MinSeg, D-(剩余可用段数)×segTarget)，保证剩余时长永远装得下，
//     因此 D ≤ MaxSegments×segTarget 时一定能规划出 ≤ MaxSegments 段（性质测试覆盖）；
//   - 对外最长时长 MaxDurationSec = floor(MaxSegments×segTarget×0.95/5)×5（默认 15/0.4/6 → 80 秒），前后端同源。
package segment

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// 切点理由（同时是 Segment.CutReason 的取值；最后一段为 ReasonEnd）。
const (
	ReasonCut   = "cut"   // 镜头切换处
	ReasonPause = "pause" // 说话停顿处
	ReasonEven  = "even"  // 没有合适的镜头切换或停顿，按时长拆分
	ReasonEnd   = "end"   // 视频结尾（最后一段）
)

// ReasonLabel 切点理由的用户文案（EXPERIENCE §4.5「切点理由」；even 为本切片新增文案，待产品确认）。
func ReasonLabel(r string) string {
	switch r {
	case ReasonCut:
		return "镜头切换处"
	case ReasonPause:
		return "说话停顿处"
	case ReasonEven:
		return "按时长拆分处"
	}
	return ""
}

// Limits 数值口径（单一数据源：GET /limits 与校验都从这里取）。
type Limits struct {
	SegCapSec      int     `json:"segCapSec"`      // 单段生成上限（秒）
	SpecMinSec     int     `json:"specMinSec"`     // 单段生成下限（秒，网关规格最短）
	OverlapSec     float64 `json:"overlapSec"`     // 拼接处多生成的时长
	MinSegSec      float64 `json:"minSegSec"`      // 非末段最短时间轴长度
	MinTailSec     float64 `json:"minTailSec"`     // 末段最短时间轴长度
	MaxSegments    int     `json:"maxSegments"`    // 段数上限
	MinDurationSec float64 `json:"minDurationSec"` // 源视频最短
	MaxDurationSec int     `json:"maxDurationSec"` // 源视频最长（由上面几项算出）
}

// DefaultLimits 默认口径：15 秒 / 0.4 秒 / 6 段 → 80 秒（DESIGN §5.17，Q1 推荐默认）。
func DefaultLimits() Limits {
	l := Limits{SegCapSec: 15, SpecMinSec: 4, OverlapSec: 0.4, MinSegSec: 3, MinTailSec: 2, MaxSegments: 6, MinDurationSec: 2}
	l.MaxDurationSec = MaxDurationFor(l.SegCapSec, l.OverlapSec, l.MaxSegments)
	return l
}

// Normalize 补默认值并按其余字段重算 MaxDurationSec（配置只给部分字段时用）。
func (l Limits) Normalize() Limits {
	d := DefaultLimits()
	if l == (Limits{}) {
		return d // 完全没配：整体用默认（否则 OverlapSec=0 会被当成有效值）
	}
	if l.SegCapSec <= 0 {
		l.SegCapSec = d.SegCapSec
	}
	if l.SpecMinSec <= 0 || l.SpecMinSec > l.SegCapSec {
		l.SpecMinSec = min(d.SpecMinSec, l.SegCapSec)
	}
	if l.OverlapSec < 0 || l.OverlapSec >= float64(l.SegCapSec)/2 {
		l.OverlapSec = d.OverlapSec
	}
	if l.MinSegSec <= 0 {
		l.MinSegSec = d.MinSegSec
	}
	if l.MinTailSec <= 0 {
		l.MinTailSec = d.MinTailSec
	}
	if l.MaxSegments <= 0 {
		l.MaxSegments = d.MaxSegments
	}
	if l.MinDurationSec <= 0 {
		l.MinDurationSec = d.MinDurationSec
	}
	l.MaxDurationSec = MaxDurationFor(l.SegCapSec, l.OverlapSec, l.MaxSegments)
	return l
}

// SegTarget 多段时每段时间轴长度上限。
func (l Limits) SegTarget() float64 { return float64(l.SegCapSec) - l.OverlapSec }

// MaxDurationFor floor(N×(cap-overlap)×0.95/5)×5：留 5% 余量吸收取整与最短尾段，再取整到 5 秒。
func MaxDurationFor(segCap int, overlap float64, maxSegments int) int {
	v := float64(maxSegments) * (float64(segCap) - overlap) * 0.95
	return int(math.Floor(v/5)) * 5
}

// Candidate 候选切点（时间点，秒）。
type Candidate struct {
	T    float64 `json:"t"`
	Kind string  `json:"kind"` // ReasonCut / ReasonPause
}

// Segment 一段。时间轴 [T0,T1] 首尾相接覆盖 [0,D]；生成窗口 [GenT0,GenT1]（含拼接处多生成的部分）。
type Segment struct {
	Index       int     `json:"index"` // 从 1 起
	T0          float64 `json:"t0"`
	T1          float64 `json:"t1"`
	GenT0       float64 `json:"genT0"`
	GenT1       float64 `json:"genT1"`
	GenDuration int     `json:"genDuration"` // 计费与网关 duration_s 用的整数秒
	OverlapIn   float64 `json:"overlapIn"`   // 与上一段的拼接处多生成的时长（第 1 段为 0）
	CutReason   string  `json:"cutReason"`   // 本段结尾的切点理由
}

// ErrTooLong 源视频超过 MaxDurationSec（或在约束下无法装进 MaxSegments 段）。
var ErrTooLong = errors.New("segment: 视频过长，无法在段数上限内拆分")

// ErrTooShort 源视频短于 MinDurationSec。
var ErrTooShort = errors.New("segment: 视频过短")

const eps = 1e-6

// Plan 规划分段。candidates 可乱序、可重复、可越界（会被过滤）。
func Plan(duration float64, candidates []Candidate, l Limits) ([]Segment, error) {
	l = l.Normalize()
	if duration < l.MinDurationSec-eps {
		return nil, ErrTooShort
	}
	if duration > float64(l.MaxDurationSec)+eps {
		return nil, fmt.Errorf("%w: %.1f 秒 > %d 秒", ErrTooLong, duration, l.MaxDurationSec)
	}
	segCap := float64(l.SegCapSec)
	if duration <= segCap+eps {
		return []Segment{finish(Segment{Index: 1, T0: 0, T1: duration, GenT0: 0, GenT1: duration, CutReason: ReasonEnd}, l)}, nil
	}
	cands := clean(candidates, duration)
	target := l.SegTarget()
	var out []Segment
	s := 0.0
	for k := 1; ; k++ {
		remain := duration - s
		if remain <= target+eps {
			out = append(out, seg(k, s, duration, ReasonEnd, l))
			return out, nil
		}
		left := l.MaxSegments - k // 本段之后还能用的段数
		if left <= 0 {
			return nil, ErrTooLong
		}
		lower := math.Max(s+l.MinSegSec, duration-float64(left)*target)
		upper := math.Min(s+target, duration-l.MinTailSec)
		if lower > upper+eps {
			return nil, ErrTooLong
		}
		cut, reason := choose(cands, s, lower, upper, remain, target)
		out = append(out, seg(k, s, cut, reason, l))
		s = cut
	}
}

// choose 在 [lower, upper] 内选切点：先在贴近上限的回看窗口里找，再放宽到整个区间；都没有就均分。
func choose(cands []Candidate, s, lower, upper, remain, target float64) (float64, string) {
	look := math.Min(4, 0.3*target)
	winLo := math.Max(lower, upper-look)
	for _, rng := range [][2]float64{{winLo, upper}, {lower, upper}} {
		for _, kind := range []string{ReasonCut, ReasonPause} {
			if t, ok := latest(cands, kind, rng[0], rng[1]); ok {
				return t, kind
			}
		}
	}
	n := math.Ceil(remain/target - eps)
	t := round2(s + remain/n)
	return clamp(t, lower, upper), ReasonEven
}

func latest(cands []Candidate, kind string, lo, hi float64) (float64, bool) {
	best, ok := 0.0, false
	for _, c := range cands {
		if c.Kind == kind && c.T >= lo-eps && c.T <= hi+eps {
			best, ok = c.T, true // cands 已按时间升序
		}
	}
	return clamp(best, lo, hi), ok
}

func seg(k int, t0, t1 float64, reason string, l Limits) Segment {
	s := Segment{Index: k, T0: t0, T1: t1, GenT0: t0, GenT1: t1, CutReason: reason}
	if k > 1 {
		s.OverlapIn = l.OverlapSec
		s.GenT0 = round2(math.Max(0, t0-l.OverlapSec))
	}
	return finish(s, l)
}

func finish(s Segment, l Limits) Segment {
	s.GenDuration = int(math.Ceil(s.GenT1 - s.GenT0 - eps))
	s.GenDuration = max(l.SpecMinSec, min(s.GenDuration, l.SegCapSec))
	return s
}

func clean(in []Candidate, duration float64) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if (c.Kind == ReasonCut || c.Kind == ReasonPause) && c.T > eps && c.T < duration-eps && !math.IsNaN(c.T) {
			out = append(out, Candidate{T: round2(c.T), Kind: c.Kind})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
