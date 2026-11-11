package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryLibraryCopyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryLibraryCopyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryLibraryCopyLogic {
	return &RetryLibraryCopyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RetryLibraryCopyLogic) retryCooldown() int {
	if n := l.svcCtx.Config.Library.RetryCooldownSeconds; n > 0 {
		return n
	}
	return 60
}

// RetryLibraryCopy 用户「重新存入素材库」（POST /api/v1/studio/tasks/{task_id}/library-retry）：付费成品转存永久失败
// （library_status=failed：子任务成功、成品仍是 mr://、该引用对应的 outbox 行是永久失败 / 作废）时把 outbox 行重置为待转存
// （attempt 进下一代、本代次数从头算，next_at=NOW），并立即交给本进程的转存池（没有池 / 名额满时由扫描到点接手）。返回该子任务（同 GetTask）。
//
//   - 归属：只认本人、未删除的子任务（与 GetTask 同一判据）。
//   - 幂等：已在转存中（pending）或已在素材库（空）直接返回当前状态；并发的两次重试只有一次改到行，另一次按当前状态返回。
//   - 限频：outbox 行进终态不足 Library.RetryCooldownSeconds（默认 60）秒的不重置，回 4002「操作太频繁」（判据在库里，多实例一致）。
//   - 结算不动：转存与计费无关，重试不冻结、不扣点。
func (l *RetryLibraryCopyLogic) RetryLibraryCopy(in *studio.GetTaskReq) (*studio.TaskInfo, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	taskID := strings.TrimSpace(in.TaskId)
	if taskID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	r := l.svcCtx.Repo
	t, err := r.Task.GetByTaskID(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil || t.UserID != in.UserId || t.Deleted != 0 {
		return nil, xerr.NotFoundf("任务")
	}
	ref := t.ResultURL.String
	if t.Status == model.TaskSucceeded && isPendingRef(ref) {
		j, err := r.CopyJob.LatestByRef(l.ctx, t.ID, ref)
		if err != nil {
			return nil, err
		}
		if j != nil && (j.Status == model.CopyFailed || j.Status == model.CopyVoid) {
			ok, err := r.CopyJob.ResetFailed(l.ctx, j.ID, l.retryCooldown())
			if err != nil {
				return nil, err
			}
			if !ok {
				// 没改到：冷却中，或并发的另一次重试刚重置过（行已是待转存 → 按幂等成功返回）
				j2, err := r.CopyJob.LatestByRef(l.ctx, t.ID, ref)
				if err != nil {
					return nil, err
				}
				if j2 == nil || j2.Status != model.CopyPending {
					return nil, xerr.New(xerr.Conflict, "操作太频繁，请稍后再试（每分钟最多一次）")
				}
			} else {
				l.Infow("studio: 用户重试转存素材库", logx.Field("task_id", taskID), logx.Field("user_id", in.UserId), logx.Field("copy_job", j.ID))
			}
			if l.svcCtx.KickCopy != nil {
				l.svcCtx.KickCopy(t.ID)
			}
		}
	}
	return NewGetTaskLogic(l.ctx, l.svcCtx).GetTask(&studio.GetTaskReq{TaskId: taskID, UserId: in.UserId})
}
