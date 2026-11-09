package logic

import (
	"context"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelBatchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelBatchLogic {
	return &CancelBatchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消批次：停止跟踪，未开始的子任务退款（gateway cancel 尽力而为）
func (l *CancelBatchLogic) CancelBatch(in *studio.BatchUserReq) (*studio.CancelBatchResp, error) {
	if err := checkBatchUser(in.BatchId, in.UserId); err != nil {
		return nil, err
	}
	d := newDeps(l.svcCtx)
	b, canceled, running, err := d.cancel(l.ctx, in.BatchId, in.UserId)
	if err != nil {
		return nil, err
	}
	if running > 0 {
		// 已提交 gateway 的不取消（结果照常出、照常结算），由轮询器收敛批次终态
		l.Infof("studio: 批次 %d 取消 %d 个排队子任务，%d 个生成中的保留", b.ID, canceled, running)
	}
	tasks, err := d.repo.Task.ListByBatch(l.ctx, b.ID)
	if err != nil {
		return nil, err
	}
	info := toBatchInfo(b, tasks)
	signViews(l.ctx, l.svcCtx, []*studio.BatchInfo{info}, nil)
	return &studio.CancelBatchResp{Batch: info, CanceledTasks: int32(canceled)}, nil
}
