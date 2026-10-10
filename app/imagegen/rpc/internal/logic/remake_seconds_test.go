package logic

import "testing"

func TestRemakeSeconds(t *testing.T) {
	cases := []struct {
		parsed float64
		script string
		want   int
	}{
		{15, "", 15},
		{0, "【总时长】：5.0 秒\n【画幅】", 5}, // 70 没回时长（2026-10-10 测试服）
		{0, "【总时长】：14.9 秒", 15},
		{0, "没有时长", 4}, // 兜底最短档，不能是 1
		{2.2, "", 4},   // 太短的原片按最短档
	}
	for _, c := range cases {
		if got := remakeSeconds(c.parsed, c.script); got != c.want {
			t.Errorf("remakeSeconds(%v, %q) = %d, want %d", c.parsed, c.script, got, c.want)
		}
	}
	if clampRemakeSeconds(1) != 4 || clampRemakeSeconds(9) != 9 {
		t.Error("clampRemakeSeconds")
	}
}
