package aigc

import "time"

// PastDeadline 判断从 since 起算是否已超过 deadline。
// 轮询里「供应商已成功、转存持续失败」的分支用它决定：未超时继续重试，超时则判失败退点，避免任务永远卡在生成中。
func PastDeadline(since time.Time, deadline time.Duration) bool {
	return time.Since(since) > deadline
}
