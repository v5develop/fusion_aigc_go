package logic

// 用户「重新存入素材库」（POST /api/v1/canvas/runs/{request_id}/library-retry）：成功 run 的产物转存永久失败
// （library_status=failed，outbox 行 status=2）时，把 outbox 行重置为待转存（attempt 进下一代、本代次数从头算，next_at=NOW）、去掉 run 上的
// 失败标记，并立即交给本进程的转存池（没有池 / 名额满时由扫描到点接手）。
//
//   - 归属：只认本人、未删除的 run（与 GET /runs/{id} 同一判据）。
//   - 幂等：已在转存中（pending）或已在素材库（空）直接返回当前状态，不报错；并发的两次重试只有一次改到行，另一次重读后按 pending 返回。
//   - 限频：outbox 行进终态（finished_at）不足 Library.RetryCooldownSeconds（默认 60）秒的不重置，回 4002「操作太频繁」。
//     每次重置至少跑完一轮才可能再进永久失败，所以同一 run 至多约每分钟重置一次；判据在库里（NOW(3)），多实例一致。
//   - 结算不动：转存与计费无关（产物早已交付、已确认），重试不冻结、不扣点。

import (
	"context"
	"errors"
	"fmt"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// errRetryCooldown 冷却中（事务内用来回滚，不对外）。
var errRetryCooldown = xerr.New(xerr.Conflict, "操作太频繁，请稍后再试（每分钟最多一次）")

func (c *core) copyRetryCooldown() int {
	if n := c.s.Config.Library.RetryCooldownSeconds; n > 0 {
		return n
	}
	return 60
}

func (c *core) RetryLibraryCopy(ctx context.Context, in *canvas.RunIdReq) (*canvas.RunResult, error) {
	r, err := c.ownedRun(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if libraryStatus(r) != LibraryFailed {
		return c.toResult(ctx, r), nil // 已在素材库 / 转存中 / 不适用：幂等返回当前状态
	}
	j, err := c.s.Repo.CopyJob.GetByRun(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		// 失败标记只由 outbox 的永久失败写入，正常不会没有行（人工删了行才会走到）：能走 outbox 就补一行，否则不支持重试
		if !c.asyncCopyOn(r) {
			return nil, xerr.New(xerr.Conflict, "暂时无法重新存入素材库，请联系客服")
		}
		err = c.s.Repo.RunInTx(ctx, func(ctx context.Context, tx *repo.Repo) error {
			if err := tx.CopyJob.Enqueue(ctx, r.ID, r.TaskID.String, r.UserID, 0); err != nil {
				return err
			}
			return tx.Run.ClearCopyFailed(ctx, r.ID)
		})
	} else {
		switch j.Status {
		case model.CopyFailed:
			err = c.s.Repo.RunInTx(ctx, func(ctx context.Context, tx *repo.Repo) error {
				ok, err := tx.CopyJob.ResetFailed(ctx, j.ID, c.copyRetryCooldown())
				if err != nil {
					return err
				}
				if !ok {
					return errRetryCooldown
				}
				return tx.Run.ClearCopyFailed(ctx, r.ID)
			})
			if errors.Is(err, errRetryCooldown) {
				// 没改到：冷却中，或并发的另一次重试刚重置过（此时行已是待转存，按幂等成功返回）
				if j2, gerr := c.s.Repo.CopyJob.GetByRun(ctx, r.ID); gerr == nil && j2 != nil && j2.Status == model.CopyPending {
					err = nil
				}
			}
		case model.CopyPending:
			// 行已在转存中、run 上却还留着失败标记：只清标记。终态与标记由 repo.FailWithRunMark 同事务写、重置与清标记也同事务，
			// 正常不会看到这种组合；只剩上线前旧代码留下的（旧持有者认领已丢仍打了标记），清掉后由在跑的这一轮给出结局
			err = c.s.Repo.Run.ClearCopyFailed(ctx, r.ID)
		default:
			return nil, xerr.New(xerr.Conflict, "转存状态已变化，请刷新")
		}
	}
	if err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infow(fmt.Sprintf("canvas: 用户重试 run %d 转存素材库", r.ID),
		logx.Field("task_id", r.TaskID.String), logx.Field("user_id", r.UserID))
	c.kickCopy(r)
	if got, gerr := c.s.Repo.Run.Get(ctx, r.ID); gerr == nil && got != nil {
		r = got
	}
	return c.toResult(ctx, r), nil
}
