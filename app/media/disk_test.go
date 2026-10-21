package media

import "testing"

// 配置值小于磁盘 30% 时取配置值；配置值为 0 时取磁盘 30%；磁盘探测失败不影响配置。
func TestEffectiveCacheCap(t *testing.T) {
	dir := t.TempDir()
	const tenGiB = int64(10) << 30
	if got := effectiveCacheCap(tenGiB, dir); got > tenGiB {
		t.Fatalf("cap %d 超过配置值 %d", got, tenGiB)
	}
	if got := effectiveCacheCap(1, dir); got != 1 {
		t.Fatalf("小配置应原样保留，got %d", got)
	}
	if got := effectiveCacheCap(tenGiB, "/nonexistent-dir-for-test"); got != tenGiB {
		t.Fatalf("探测失败应退回配置值，got %d", got)
	}
	if got := effectiveCacheCap(0, dir); got <= 0 {
		t.Fatalf("配置为 0 时应取磁盘 30%%，got %d", got)
	}
}
