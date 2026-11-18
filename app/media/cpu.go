package media

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// effectiveCPUs 本进程可用的有效核数：min(GOMAXPROCS, cgroup CPU 配额)。
// runtime.NumCPU 看的是宿主机核数，容器限制了 CPU 时会严重偏大；Go 1.25+ 的 GOMAXPROCS 默认已读 cgroup，
// 但可被 GOMAXPROCS / GODEBUG 覆盖或关闭，所以这里再显式读一次配额取较小值。
func effectiveCPUs() int {
	return effectiveCPUsFrom(runtime.GOMAXPROCS(0), os.ReadFile)
}

// effectiveCPUsFrom readFile 可注入（测试用）。读不到配额按不限制处理。
func effectiveCPUsFrom(maxprocs int, readFile func(string) ([]byte, error)) int {
	n := max(1, maxprocs)
	if q, ok := cgroupCPUQuota(readFile); ok {
		n = min(n, max(1, int(math.Ceil(q))))
	}
	return n
}

// cgroupCPUQuota 读 cgroup v2 的 cpu.max（"<quota|max> <period>"），不行再读 v1 的 cfs_quota_us / cfs_period_us。
// 返回配额折算的核数（quota/period）；无限制或读不到返回 false。
func cgroupCPUQuota(readFile func(string) ([]byte, error)) (float64, bool) {
	if raw, err := readFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(raw))
		if len(f) >= 1 {
			if f[0] == "max" {
				return 0, false
			}
			period := 100000.0
			if len(f) >= 2 {
				if p, err := strconv.ParseFloat(f[1], 64); err == nil && p > 0 {
					period = p
				}
			}
			if q, err := strconv.ParseFloat(f[0], 64); err == nil && q > 0 {
				return q / period, true
			}
		}
		return 0, false
	}
	q, err1 := readInt(readFile, "/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p, err2 := readInt(readFile, "/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0, false
	}
	return float64(q) / float64(p), true
}

func readInt(readFile func(string) ([]byte, error), path string) (int64, error) {
	raw, err := readFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}
