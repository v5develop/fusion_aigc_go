package asset

// 转存素材库永久失败的定时重投（BACKLOG C2 / S2：画布 afv_canvas_copy_job、studio afv_studio_copy_job 共用口径）。
//
// 永久失败（library_status=failed）的 outbox 行按「代」重投：attempt 每重投一次跳到下一个整千（与用户一键重试同一机制，
// 见各自 repo.CopyAttemptEpoch），本代再按 Library.MaxAttempts / BackoffSeconds 跑满；第 g 代（0 起）永久失败后等
// AutoRequeueBackoffSeconds[g] 秒再重投，代数到 AutoRequeueMax 后不再自动重投（用户仍可一键重试）。
//
// 日志判据：
//   - ASSET_LIBRARY_RETRY_REQUEUED  INFO  已自动重投
//   - ASSET_LIBRARY_RETRY_EXHAUSTED ERROR 永久失败且自动重投已用尽（告警：需人工看，用户仍可一键重试）
//   - ASSET_LIBRARY_RETRY_FAILED    ERROR 重投扫描 / 改行出错（下一轮再试）

import (
	"strings"
)

// 定时重投日志判据。
const (
	CheckLibraryRetryRequeued  = "ASSET_LIBRARY_RETRY_REQUEUED"
	CheckLibraryRetryExhausted = "ASSET_LIBRARY_RETRY_EXHAUSTED"
	CheckLibraryRetryFailed    = "ASSET_LIBRARY_RETRY_FAILED"
)

// DefaultRequeueBackoff 第 g 代永久失败后等多久重投（秒）：30 分钟、1、2、6、12 小时。
var DefaultRequeueBackoff = []int{1800, 3600, 7200, 21600, 43200}

// RequeueBackoff 第 gen 代（0 起）永久失败后的重投等待秒数（超出取最后一项；list 为空用默认）。
func RequeueBackoff(list []int, gen int) int {
	if len(list) == 0 {
		list = DefaultRequeueBackoff
	}
	gen = max(gen, 0)
	if gen >= len(list) {
		gen = len(list) - 1
	}
	return max(1, list[gen])
}

// RequeueDueCond 「已过本代重投等待」的 SQL 条件与参数：finished_at <= NOW(3) - INTERVAL (按 attempt DIV epoch 取等待秒数) SECOND。
// maxGen 为代数上限（只需展开 0..maxGen-1 代）。列名为字面量 attempt / finished_at。
func RequeueDueCond(list []int, maxGen int, epoch int32) (string, []any) {
	var b strings.Builder
	args := []any{epoch}
	b.WriteString("finished_at IS NOT NULL AND finished_at <= NOW(3) - INTERVAL (CASE attempt DIV ?")
	for g := 0; g < maxGen; g++ {
		b.WriteString(" WHEN ? THEN ?")
		args = append(args, g, RequeueBackoff(list, g))
	}
	b.WriteString(" ELSE ? END) SECOND")
	args = append(args, RequeueBackoff(list, maxGen))
	return b.String(), args
}
