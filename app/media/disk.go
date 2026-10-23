package media

import "syscall"

// cacheDiskShare 输入缓存最多占用磁盘可用空间的比例（百分比）。
const cacheDiskShare = 30

// effectiveCacheCap 缓存的实际上限：配置值与“磁盘可用空间的 30%”取较小者。
// 读不到磁盘信息时退回配置值，不因为探测失败而关闭缓存。
func effectiveCacheCap(configured int64, dir string) int64 {
	return effectiveCacheCapShare(configured, dir, cacheDiskShare)
}

// effectiveCacheCapShare 同 effectiveCacheCap，磁盘占比由调用方给（对象缓存用 objectCacheDiskShare）。
func effectiveCacheCapShare(configured int64, dir string, sharePercent int64) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return configured
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if share := free * sharePercent / 100; configured <= 0 || share < configured {
		return share
	}
	return configured
}
