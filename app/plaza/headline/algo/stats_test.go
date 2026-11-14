package algo

import (
	"math"
	"testing"
	"time"
)

// 固定时钟：2026-11-13 14:30（周五，CST）。
var now0 = time.Date(2026, 11, 13, 14, 30, 0, 0, CST)

// hoursAgo 构造小时桶：键是「当前小时往前 n 个小时」，值为次数。
func hoursAgo(now time.Time, m map[int]int) map[int64]int {
	out := map[int64]int{}
	cur := HourKey(now)
	for n, c := range m {
		out[cur-int64(n)*3600] += c
	}
	return out
}

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func TestComputeStatsWindows(t *testing.T) {
	p := DefaultParams()
	sig := Signal{TemplateID: 1, UniqueUsers: 4, Hours: hoursAgo(now0, map[int]int{
		0:   2, // 当前小时（r24、7 天）
		23:  1, // r24 最后一个桶
		24:  3, // 7 天但不在 r24
		167: 4, // 7 天最后一个桶
		168: 5, // 前 7 天第一个桶
		335: 6, // 前 7 天最后一个桶
		336: 9, // 窗口外，忽略
		-1:  7, // 未来桶，忽略
	})}
	st := ComputeStats(sig, now0.AddDate(0, 0, -30), now0, p)
	if st.Uses7d != 10 || st.R24 != 3 || st.PrevUses != 11 || st.UniqueUsers != 4 {
		t.Fatalf("窗口统计错误: %+v", st)
	}
	if !near(st.Rate6d, float64(10-3)/6, 1e-4) {
		t.Fatalf("rate6d=%v", st.Rate6d)
	}
}

func TestDecayUsesBucketMidpointAndHalfLife(t *testing.T) {
	p := DefaultParams()
	// now 是 14:30，当前小时桶中点 14:30 → 年龄 0，权重 1
	st := ComputeStats(Signal{Hours: hoursAgo(now0, map[int]int{0: 1})}, time.Time{}, now0, p)
	if !near(st.Decayed, 1, 1e-4) {
		t.Fatalf("当前桶衰减应为 1, got %v", st.Decayed)
	}
	// 24 小时前的桶：年龄正好 24h → 0.5（半衰期 24h）
	st = ComputeStats(Signal{Hours: hoursAgo(now0, map[int]int{24: 1})}, time.Time{}, now0, p)
	if !near(st.Decayed, 0.5, 1e-4) {
		t.Fatalf("24h 前应减半, got %v", st.Decayed)
	}
	// 48 小时前 → 0.25；改半衰期 12h 时 24 小时前 → 0.25
	st = ComputeStats(Signal{Hours: hoursAgo(now0, map[int]int{48: 4})}, time.Time{}, now0, p)
	if !near(st.Decayed, 1, 1e-4) {
		t.Fatalf("48h 前 4 次应为 1, got %v", st.Decayed)
	}
	p.HalfLifeHours = 12
	st = ComputeStats(Signal{Hours: hoursAgo(now0, map[int]int{24: 4})}, time.Time{}, now0, p)
	if !near(st.Decayed, 1, 1e-4) {
		t.Fatalf("半衰期 12h 时 24h 前 4 次应为 1, got %v", st.Decayed)
	}
	// 当前小时前半段：桶中点晚于 now，年龄按 0 计，不出现 >1 的权重
	early := time.Date(2026, 11, 13, 14, 5, 0, 0, CST)
	st = ComputeStats(Signal{Hours: hoursAgo(early, map[int]int{0: 1})}, time.Time{}, early, DefaultParams())
	if st.Decayed != 1 {
		t.Fatalf("未来中点应按 0 计, got %v", st.Decayed)
	}
}

func TestVelocityMinBaseAndClamp(t *testing.T) {
	p := DefaultParams()
	// 前 6 天共 3 次（日均 0.5），今天 3 次：不是 +500%，而是 (3−0.5)/3 = 0.8333
	sig := Signal{Hours: hoursAgo(now0, map[int]int{2: 3, 30: 1, 60: 1, 100: 1})}
	st := ComputeStats(sig, time.Time{}, now0, p)
	if !near(st.Velocity, (3-0.5)/3, 1e-4) {
		t.Fatalf("最小基数失效: velocity=%v", st.Velocity)
	}
	// 大基数正常计算：前 6 天 60 次（日均 10），今天 40 次 → (40−10)/10 = 3 → 夹到上限 2
	sig = Signal{Hours: hoursAgo(now0, map[int]int{1: 40, 50: 60})}
	if st = ComputeStats(sig, time.Time{}, now0, p); st.Velocity != 2 {
		t.Fatalf("应夹到 velocity.max=2, got %v", st.Velocity)
	}
	// 今天 0 次、前 6 天 60 次 → (0−10)/10 = −1 → 夹到 −1
	sig = Signal{Hours: hoursAgo(now0, map[int]int{50: 60})}
	if st = ComputeStats(sig, time.Time{}, now0, p); st.Velocity != -1 {
		t.Fatalf("应夹到 velocity.min=-1, got %v", st.Velocity)
	}
	p.Velocity.Min = -0.5
	if st = ComputeStats(sig, time.Time{}, now0, p); st.Velocity != -0.5 {
		t.Fatalf("应夹到 -0.5, got %v", st.Velocity)
	}
}

func TestFreshnessBoundaries(t *testing.T) {
	if f := Freshness(0, 14); f != 1 {
		t.Fatalf("当天上架应为 1, got %v", f)
	}
	if f := Freshness(14, 14); f != 0 {
		t.Fatalf("ageDays = freshDays 应为 0, got %v", f)
	}
	if f := Freshness(30, 14); f != 0 {
		t.Fatalf("超过应为 0, got %v", f)
	}
	if f := Freshness(0, 0); f != 0 {
		t.Fatalf("freshDays=0 恒为 0, got %v", f)
	}
	if f := Freshness(7, 14); f != 0.5 {
		t.Fatalf("一半应为 0.5, got %v", f)
	}
	// 整天数按 CST 自然日：昨天 23:59 创建、今天 00:01 计算 → 1 天
	created := time.Date(2026, 11, 12, 23, 59, 0, 0, CST)
	if d := AgeDays(created, time.Date(2026, 11, 13, 0, 1, 0, 0, CST)); d != 1 {
		t.Fatalf("ageDays=%d", d)
	}
	// UTC 表示的同一时刻也按 CST 取日
	if d := AgeDays(created.UTC(), time.Date(2026, 11, 13, 0, 1, 0, 0, CST).UTC()); d != 1 {
		t.Fatalf("UTC 输入 ageDays=%d", d)
	}
	if d := AgeDays(now0.Add(time.Hour), now0); d != 0 {
		t.Fatalf("未来创建应夹到 0, got %d", d)
	}
}

func TestTrendMatchesJava(t *testing.T) {
	if (Stats{Uses7d: 5}).Trend() != nil {
		t.Fatal("前 7 天为 0 时 trend 应为 nil")
	}
	if v := *(Stats{Uses7d: 186, PrevUses: 100}).Trend(); v != 86 {
		t.Fatalf("trend=%d", v)
	}
	// Java Math.round 对 −0.5 向上取整（−2.5 → −2）
	if v := *(Stats{Uses7d: 39, PrevUses: 40}).Trend(); v != -2 {
		t.Fatalf("trend=%d（应与 Java Math.round 一致）", v)
	}
	if !(Stats{Uses7d: 3}).IsNew() || (Stats{PrevUses: 1}).IsNew() {
		t.Fatal("isNew 口径错误")
	}
}

func TestScoreDetailAndRegistry(t *testing.T) {
	p := DefaultParams()
	st := Stats{Decayed: math.E - 1, Velocity: 0.5, Freshness: 0.25, UniqueUsers: 0}
	score, detail := Score(st, p, ScoreContext{})
	want := 1.0*1 + 0.6*0.5 + 0.8*0.25 + 0.5*0
	if !near(score, want, 1e-4) {
		t.Fatalf("score=%v want %v", score, want)
	}
	if detail["ctr"].Raw != nil || detail["ctr"].V != 0 {
		t.Fatalf("ctr 未落地应为 raw=null: %+v", detail["ctr"])
	}
	if d := detail["decay"]; d.Raw == nil || !near(*d.Raw, 1, 1e-4) || d.W != 1 {
		t.Fatalf("decay 明细错误: %+v", d)
	}
	// 去重用户数参与打分：同样的热度，用户更多的分更高
	a, _ := Score(Stats{Decayed: 5, UniqueUsers: 1}, p, ScoreContext{})
	b, _ := Score(Stats{Decayed: 5, UniqueUsers: 9}, p, ScoreContext{})
	if b <= a {
		t.Fatalf("用户数应加分: %v <= %v", b, a)
	}
	// 按范围归一化：decay 项除以 ln(1+maxDecayed) 再乘 norm-scale
	p.ScoreNormalize = NormalizeScopeMax
	p.NormScale = 2
	_, detail = Score(Stats{Decayed: math.E - 1}, p, ScoreContext{MaxDecayed: math.E - 1})
	if !near(*detail["decay"].Raw, 2, 1e-4) {
		t.Fatalf("归一化后应为 2, got %v", *detail["decay"].Raw)
	}
	if got := UnknownWeightKeys([]byte(`{"global":{"weight":{"decay":1,"foo":2,"bar":0}}}`)); len(got) != 2 || got[0] != "bar" || got[1] != "foo" {
		t.Fatalf("未知信号键: %v", got)
	}
}
