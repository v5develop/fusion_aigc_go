package segment

import (
	"errors"
	"math"
	"math/rand"
	"testing"
	"testing/quick"
)

func TestDefaultLimits80Seconds(t *testing.T) {
	l := DefaultLimits()
	if l.MaxDurationSec != 80 || l.MaxSegments != 6 || l.SegCapSec != 15 {
		t.Fatalf("默认口径应为 80 秒 / 6 段 / 15 秒：%+v", l)
	}
	if MaxDurationFor(15, 0.4, 7) != 95 {
		t.Fatalf("7 段应为 95 秒，得 %d", MaxDurationFor(15, 0.4, 7))
	}
}

func TestSingleSegment(t *testing.T) {
	segs, err := Plan(12.3, nil, DefaultLimits())
	if err != nil || len(segs) != 1 {
		t.Fatalf("segs=%+v err=%v", segs, err)
	}
	s := segs[0]
	if s.T0 != 0 || s.T1 != 12.3 || s.GenDuration != 13 || s.OverlapIn != 0 || s.CutReason != ReasonEnd {
		t.Fatalf("单段不对：%+v", s)
	}
	// 15 秒整也是单段（没有拼接）
	if segs, _ := Plan(15, nil, DefaultLimits()); len(segs) != 1 || segs[0].GenDuration != 15 {
		t.Fatalf("15 秒应单段：%+v", segs)
	}
	// 3 秒视频生成最短按 4 秒
	if segs, _ := Plan(3, nil, DefaultLimits()); segs[0].GenDuration != 4 {
		t.Fatalf("短视频应夹到 4 秒：%+v", segs)
	}
}

func TestBounds(t *testing.T) {
	if _, err := Plan(1.5, nil, DefaultLimits()); !errors.Is(err, ErrTooShort) {
		t.Fatalf("太短应拒：%v", err)
	}
	if _, err := Plan(80.5, nil, DefaultLimits()); !errors.Is(err, ErrTooLong) {
		t.Fatalf("超过 80 秒应拒：%v", err)
	}
	if segs, err := Plan(80, nil, DefaultLimits()); err != nil || len(segs) > 6 {
		t.Fatalf("80 秒应可规划：%v %d", err, len(segs))
	}
}

func TestPrefersCutOverPause(t *testing.T) {
	// 32 秒：窗口 [10.6,14.6] 内同时有停顿 14.0 和镜头切换 12.0，选镜头切换
	cands := []Candidate{{T: 14.0, Kind: ReasonPause}, {T: 12.0, Kind: ReasonCut}, {T: 25, Kind: ReasonCut}}
	segs, err := Plan(32, cands, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].T1 != 12.0 || segs[0].CutReason != ReasonCut {
		t.Fatalf("第 1 段应切在镜头切换 12.0：%+v", segs[0])
	}
	if segs[1].T0 != 12.0 || segs[1].GenT0 != 11.6 || segs[1].OverlapIn != 0.4 {
		t.Fatalf("第 2 段生成窗口应前移 0.4 秒：%+v", segs[1])
	}
	if segs[1].T1 != 25 || segs[1].CutReason != ReasonCut {
		t.Fatalf("第 2 段应切在 25：%+v", segs[1])
	}
	if last := segs[len(segs)-1]; last.T1 != 32 || last.CutReason != ReasonEnd {
		t.Fatalf("末段应到结尾：%+v", last)
	}
}

func TestPauseWhenNoCut(t *testing.T) {
	segs, _ := Plan(28, []Candidate{{T: 13.5, Kind: ReasonPause}}, DefaultLimits())
	if segs[0].T1 != 13.5 || segs[0].CutReason != ReasonPause {
		t.Fatalf("应切在停顿：%+v", segs)
	}
}

func TestEvenSplitWithoutCandidates(t *testing.T) {
	segs, err := Plan(40, nil, DefaultLimits())
	if err != nil || len(segs) != 3 {
		t.Fatalf("40 秒无候选应均分 3 段：%+v %v", segs, err)
	}
	for _, s := range segs[:2] {
		if s.CutReason != ReasonEven {
			t.Fatalf("应是均分：%+v", s)
		}
	}
}

// 可行性约束：候选切点全挤在很早的位置时，贪心若取早切点会超段数；约束保证仍 ≤ 6 段。
func TestFeasibilityConstraint(t *testing.T) {
	var cands []Candidate
	for x := 3.0; x < 80; x += 11.0 { // 每 11 秒一个镜头切换，贪心取它会切出 8 段
		cands = append(cands, Candidate{T: x, Kind: ReasonCut})
	}
	segs, err := Plan(80, cands, DefaultLimits())
	if err != nil || len(segs) > 6 {
		t.Fatalf("应 ≤6 段：n=%d err=%v", len(segs), err)
	}
	checkInvariants(t, 80, segs, DefaultLimits())
}

// 性质测试：D ≤ MaxDurationSec 时一定能规划且满足全部不变量。
func TestPropertyInvariants(t *testing.T) {
	l := DefaultLimits()
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		d := l.MinDurationSec + r.Float64()*(float64(l.MaxDurationSec)-l.MinDurationSec)
		d = math.Round(d*100) / 100
		n := r.Intn(30)
		cands := make([]Candidate, n)
		for i := range cands {
			k := ReasonCut
			if r.Intn(2) == 0 {
				k = ReasonPause
			}
			cands[i] = Candidate{T: r.Float64() * d * 1.1, Kind: k}
		}
		segs, err := Plan(d, cands, l)
		if err != nil {
			t.Logf("d=%.2f err=%v", d, err)
			return false
		}
		return checkInvariants(t, d, segs, l)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
}

func TestDeterministic(t *testing.T) {
	cands := []Candidate{{T: 20, Kind: ReasonPause}, {T: 9, Kind: ReasonCut}, {T: 44, Kind: ReasonCut}}
	a, _ := Plan(61.2, cands, DefaultLimits())
	b, _ := Plan(61.2, []Candidate{cands[2], cands[0], cands[1]}, DefaultLimits())
	if len(a) != len(b) {
		t.Fatal("同输入应同输出")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同输入应同输出：%+v vs %+v", a[i], b[i])
		}
	}
}

func checkInvariants(t *testing.T, d float64, segs []Segment, l Limits) bool {
	t.Helper()
	if len(segs) == 0 || len(segs) > l.MaxSegments {
		t.Logf("段数越界 %d", len(segs))
		return false
	}
	if segs[0].T0 != 0 || math.Abs(segs[len(segs)-1].T1-d) > 1e-9 {
		t.Logf("未覆盖 [0,D]：%+v", segs)
		return false
	}
	for i, s := range segs {
		if s.Index != i+1 {
			return false
		}
		if i > 0 && s.T0 != segs[i-1].T1 {
			t.Logf("不连续：%+v", segs)
			return false
		}
		if s.GenDuration < l.SpecMinSec || s.GenDuration > l.SegCapSec {
			t.Logf("生成时长越界：%+v", s)
			return false
		}
		if s.GenT1-s.GenT0 > float64(l.SegCapSec)+1e-6 {
			t.Logf("生成窗口超上限：%+v", s)
			return false
		}
		if len(segs) > 1 {
			if i < len(segs)-1 && s.T1-s.T0 < l.MinSegSec-1e-6 {
				t.Logf("非末段过短：%+v", s)
				return false
			}
			if i == len(segs)-1 && s.T1-s.T0 < l.MinTailSec-1e-6 {
				t.Logf("末段过短：%+v", segs)
				return false
			}
		}
	}
	return true
}
