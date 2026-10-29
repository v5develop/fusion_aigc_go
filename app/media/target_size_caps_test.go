package media

import "testing"

// 大模型版的输入规则：短边 ≤1080 且长边 ≤1920（同时成立）。
func TestTargetSizeCapsShortAndLongSideTogether(t *testing.T) {
	rule := RuleSpec{MaxShortSide: 1080, MaxLongSide: 1920}
	cases := []struct {
		w, h int
	}{
		{2560, 1440}, // 16:9 2K：缩到 1920x1080
		{1440, 2560}, // 竖屏 2K
		{3840, 2160}, // 4K：缩到 1920x1080
		{1080, 2400}, // 高瘦屏：长边被限
		{4000, 1000}, // 超宽：短边远小于上限，长边 ≤1920
		{1200, 1200}, // 方形：短边超限
	}
	for _, c := range cases {
		tw, th, err := targetSize(c.w, c.h, rule)
		if err != nil {
			t.Fatalf("%dx%d: %v", c.w, c.h, err)
		}
		if min(tw, th) > 1080 || max(tw, th) > 1920 {
			t.Fatalf("%dx%d -> %dx%d breaks the caps", c.w, c.h, tw, th)
		}
		if tw%2 != 0 || th%2 != 0 {
			t.Fatalf("%dx%d -> %dx%d not even", c.w, c.h, tw, th)
		}
	}
	if tw, th, _ := targetSize(2560, 1440, rule); tw != 1920 || th != 1080 {
		t.Fatalf("2K -> %dx%d, want 1920x1080", tw, th)
	}
}

// 没有上限的规则保持原尺寸，既有行为不变。
func TestTargetSizeWithoutCapsKeepsSize(t *testing.T) {
	if tw, th, _ := targetSize(1280, 720, RuleSpec{}); tw != 1280 || th != 720 {
		t.Fatalf("got %dx%d", tw, th)
	}
}
