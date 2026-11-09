package logic

import (
	"context"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetBatchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetBatchLogic {
	return &GetBatchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批次详情 + 全部子任务（生成中轮询此接口）
func (l *GetBatchLogic) GetBatch(in *studio.GetBatchReq) (*studio.BatchDetail, error) {
	if err := checkBatchUser(in.BatchId, in.UserId); err != nil {
		return nil, err
	}
	d := newDeps(l.svcCtx)
	b, err := d.ownedBatch(l.ctx, in.BatchId, in.UserId)
	if err != nil {
		return nil, err
	}
	tasks, err := d.repo.Task.ListByBatch(l.ctx, b.ID)
	if err != nil {
		return nil, err
	}
	out := toBatchDetail(b, tasks)
	attachAssetIDsToDetails(l.ctx, d.repo, in.UserId, out)
	if !in.RawRefs { // 内部调用方（canvas）要原始引用：自己持久化引用、读时再签，不落短期签名地址
		signDetails(l.ctx, l.svcCtx, out)
	}
	return out, nil
}
