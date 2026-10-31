package logic

import (
	"context"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteBatchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteBatchLogic {
	return &DeleteBatchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 删除批次（软删）：进行中的批次拒绝删除
func (l *DeleteBatchLogic) DeleteBatch(in *studio.BatchUserReq) (*studio.Empty, error) {
	if err := checkBatchUser(in.BatchId, in.UserId); err != nil {
		return nil, err
	}
	d := newDeps(l.svcCtx)
	b, err := d.ownedBatch(l.ctx, in.BatchId, in.UserId)
	if err != nil {
		return nil, err
	}
	counts, err := d.repo.Task.CountByBatch(l.ctx, b.ID)
	if err != nil {
		return nil, err
	}
	if !model.BatchFinal(b.Status) || counts[model.TaskQueued] > 0 || counts[model.TaskRunning] > 0 || counts[model.TaskDelivering] > 0 {
		return nil, xerr.New(xerr.Conflict, "生成中不能删除，请等待完成或先取消")
	}
	err = d.repo.RunInTx(l.ctx, func(ctx context.Context, tx *repo.Repo) error {
		if _, err := tx.Batch.SoftDelete(ctx, b.ID, in.UserId); err != nil {
			return err
		}
		return tx.Task.SoftDeleteByBatch(ctx, b.ID) // 任务中心靠同步收敛删除态
	})
	if err != nil {
		return nil, err
	}
	return &studio.Empty{}, nil
}
