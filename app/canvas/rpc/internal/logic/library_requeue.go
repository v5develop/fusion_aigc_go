package logic

// 转存永久失败的定时重投（BACKLOG C2）：library_status=failed 的 outbox 行（status=永久失败）按代退避自动重投，
// 与用户「重新存入素材库」（library_retry.go）同一机制——attempt 进下一代、去掉 run 上的失败标记，二者同事务；
// 以列出时的 attempt 做 CAS，与用户重试 / 别的实例并发时只有一个改到。代数到 Library.AutoRequeueMax 后不再自动重投
// （copyGiveUp 打 ERROR check=ASSET_LIBRARY_RETRY_EXHAUSTED），用户仍可一键重试。口径见 pkg/asset/library_retry.go。

import (
	"context"
	"fmt"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/asset"

	"github.com/zeromicro/go-zero/core/logx"
)

// requeueInterval 重投扫描间隔。
const requeueInterval = time.Minute

// requeueGen 本行当前所在的代（0 起；每次重投 / 用户重试 +1）。
func requeueGen(attempt int32) int { return int(attempt / repo.CopyAttemptEpoch) }

// autoRequeueLeft 本代永久失败后还会不会被自动重投。
func (c *core) autoRequeueLeft(attempt int32) bool {
	return requeueGen(attempt) < c.s.Config.Library.AutoRequeueMax
}

// requeueFailedCopies 扫一轮可重投的永久失败行并重投（每分钟至多一次，由 Poller.Tick 调用）。返回重投行数。
func (c *core) requeueFailedCopies(ctx context.Context) int {
	maxGen := c.s.Config.Library.AutoRequeueMax
	if maxGen <= 0 {
		return 0
	}
	log := logx.WithContext(ctx)
	cond, args := asset.RequeueDueCond(c.s.Config.Library.AutoRequeueBackoffSeconds, maxGen, repo.CopyAttemptEpoch)
	list, err := c.s.Repo.CopyJob.ListRequeueable(ctx, maxGen, cond, args, max(1, c.s.Config.Library.AutoRequeueBatch))
	if err != nil {
		if !isMissingTable(err) {
			log.Errorf("canvas: 查可重投的转存失败行出错 (check=%s): %v", asset.CheckLibraryRetryFailed, err)
		}
		return 0
	}
	n := 0
	for _, j := range list {
		ok := false
		err := c.s.Repo.RunInTx(ctx, func(ctx context.Context, tx *repo.Repo) error {
			got, err := tx.CopyJob.Requeue(ctx, j.ID, j.Attempt)
			if err != nil || !got {
				return err
			}
			if err := tx.Run.ClearCopyFailed(ctx, j.RunPK); err != nil {
				return err
			}
			ok = true
			return nil
		})
		if err != nil {
			log.Errorf("canvas: 重投转存任务 %d（run %d）出错，下一轮再试 (check=%s): %v", j.ID, j.RunPK, asset.CheckLibraryRetryFailed, err)
			continue
		}
		if !ok {
			continue // 用户已重试 / 别的实例已重投
		}
		n++
		copyJobs.Inc("requeued")
		log.Infow(fmt.Sprintf("canvas: run %d 转存素材库永久失败后自动重投（第 %d 代） (check=%s)", j.RunPK, requeueGen(j.Attempt)+1, asset.CheckLibraryRetryRequeued),
			logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID))
		c.kickCopy(&model.NodeRun{ID: j.RunPK})
	}
	return n
}
