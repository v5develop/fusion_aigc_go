// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetBatchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批次详情：切图框、组图、prompt、每步耗时与成本
func NewGetBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetBatchLogic {
	return &GetBatchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetBatchLogic) GetBatch(req *types.IdPath) (resp *types.StudioAdminBatchDetail, err error) {
	out, err := l.svcCtx.StudioRpc.AdminGetBatch(l.ctx, &studio_client.AdminGetBatchReq{BatchId: req.Id})
	if err != nil {
		return nil, err
	}
	resp = &types.StudioAdminBatchDetail{
		Batch: toAdminBatch(out.Batch), CropPlan: out.CropPlan, CropCheck: out.CropCheck,
		SelectedImages: out.SelectedImages, StepLog: out.StepLog, Tasks: make([]types.StudioAdminTask, 0, len(out.Tasks)),
	}
	for _, t := range out.Tasks {
		resp.Tasks = append(resp.Tasks, toAdminTask(t))
	}
	return resp, nil
}
