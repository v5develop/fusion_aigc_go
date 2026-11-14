package logic

// 转存永久失败的定时重投（BACKLOG S2）：library_status=failed 的 outbox 行（status=永久失败；作废不重投——作废是「成品没付费 /
// 不再是它」）按代退避自动重投，与用户「重新存入素材库」（retry_library_copy_logic.go）同一机制：attempt 进下一代、next_at=NOW；
// 以列出时的 attempt 做 CAS，与用户重试 / 别的实例并发时只有一个改到。重投后由 runCopyJob 照常判定（成品已换 / 已不是成功态的照旧作废）。
// 代数到 Library.AutoRequeueMax 后不再自动重投（giveUp 打 ERROR check=ASSET_LIBRARY_RETRY_EXHAUSTED），用户仍可一键重试。
// 口径见 pkg/asset/library_retry.go。

import (
	"context"
	"fmt"
	"time"

	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/pkg/asset"

	"github.com/zeromicro/go-zero/core/logx"
)

// requeueInterval 重投扫描间隔。
const requeueInterval = time.Minute

// requeueGen 本行当前所在的代（0 起；每次重投 / 用户重试 +1）。
func requeueGen(attempt int32) int { return int(attempt / repo.CopyAttemptEpoch) }

// autoRequeueLeft 本代永久失败后还会不会被自动重投。
func (f *flow) autoRequeueLeft(attempt int32) bool {
	return requeueGen(attempt) < f.cfg.Library.AutoRequeueMax
}

// requeueTick Poller 每轮调用：到间隔才扫（单协程调用）。
func (f *flow) requeueTick(ctx context.Context) {
	if !f.requeueAt.IsZero() && f.now().Sub(f.requeueAt) < requeueInterval {
		return
	}
	f.requeueAt = f.now()
	f.requeueFailedCopies(ctx)
}

// requeueFailedCopies 扫一轮可重投的永久失败行并重投。返回重投行数。
func (f *flow) requeueFailedCopies(ctx context.Context) int {
	maxGen := f.cfg.Library.AutoRequeueMax
	if maxGen <= 0 {
		return 0
	}
	log := logx.WithContext(ctx)
	cond, args := asset.RequeueDueCond(f.cfg.Library.AutoRequeueBackoffSeconds, maxGen, repo.CopyAttemptEpoch)
	list, err := f.repo.CopyJob.ListRequeueable(ctx, maxGen, cond, args, max(1, f.cfg.Library.AutoRequeueBatch))
	if err != nil {
		if !isMissingTable(err) {
			log.Errorf("studio: 查可重投的转存失败行出错 (check=%s): %v", asset.CheckLibraryRetryFailed, err)
		}
		return 0
	}
	n := 0
	for _, j := range list {
		ok, err := f.repo.CopyJob.Requeue(ctx, j.ID, j.Attempt)
		if err != nil {
			log.Errorf("studio: 重投转存任务 %d（子任务 %d）出错，下一轮再试 (check=%s): %v", j.ID, j.TaskPK, asset.CheckLibraryRetryFailed, err)
			continue
		}
		if !ok {
			continue // 用户已重试 / 别的实例已重投
		}
		n++
		copyJobs.Inc("requeued")
		log.Infow(fmt.Sprintf("studio: 子任务 %d 转存素材库永久失败后自动重投（第 %d 代） (check=%s)", j.TaskPK, requeueGen(j.Attempt)+1, asset.CheckLibraryRetryRequeued),
			logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID))
		f.kickCopy(j.TaskPK)
	}
	return n
}
