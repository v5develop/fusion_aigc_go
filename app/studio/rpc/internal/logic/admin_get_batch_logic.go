package logic

import (
	"context"
	"time"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminGetBatchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminGetBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminGetBatchLogic {
	return &AdminGetBatchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批次详情：含切图框、切图质检、组图、每步耗时与成本、每个子任务的完整 prompt
func (l *AdminGetBatchLogic) AdminGetBatch(in *studio.AdminGetBatchReq) (*studio.AdminBatchDetail, error) {
	if in.BatchId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少批次号")
	}
	r := l.svcCtx.Repo
	b, err := r.Batch.GetByID(l.ctx, in.BatchId, 0) // 含已删除
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, xerr.NotFoundf("批次")
	}
	tasks, err := r.Task.ListByBatch(l.ctx, b.ID)
	if err != nil {
		return nil, err
	}
	stats, err := r.Task.StatsByBatches(l.ctx, []int64{b.ID})
	if err != nil {
		return nil, err
	}
	out := &studio.AdminBatchDetail{
		Batch:    toAdminBatch(b, tasks, stats[b.ID]),
		CropPlan: jsonText(b.CropPlan), CropCheck: jsonText(b.CropCheck),
		SelectedImages: jsonText(b.SelectedImages), StepLog: jsonText(b.StepLog),
		Tasks: make([]*studio.AdminTask, 0, len(tasks)),
	}
	grace, now := secs(l.svcCtx.Config.Timeouts.DeliverGrace, defaultDeliverGrace), time.Now()
	for _, t := range tasks {
		at := toAdminTask(t, b.Status)
		if deliveryOverdue(t, now, grace) && at.Task != nil {
			// 交付中已超过宽限期：强制失败仍拒绝（结算可能已发生），在详情里透出，交给 Poller 的超期出口处理
			at.Task.ErrorReason = reasonDeliverOverdue
		}
		out.Tasks = append(out.Tasks, at)
	}
	return out, nil
}
