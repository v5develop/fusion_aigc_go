// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetBatchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批次详情 + 子任务（生成中轮询此接口）
func NewGetBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetBatchLogic {
	return &GetBatchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetBatchLogic) GetBatch(req *types.IdPath) (resp *types.StudioBatchDetail, err error) {
	out, err := l.svcCtx.StudioRpc.GetBatch(l.ctx, &studio_client.GetBatchReq{BatchId: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	d := toDetail(out)
	return &d, nil
}
