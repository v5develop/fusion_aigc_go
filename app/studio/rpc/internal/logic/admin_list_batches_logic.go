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

type AdminListBatchesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListBatchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListBatchesLogic {
	return &AdminListBatchesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 运营后台 ----
func (l *AdminListBatchesLogic) AdminListBatches(in *studio.AdminListBatchesReq) (*studio.AdminListBatchesResp, error) {
	if err := checkKind(in.Kind); err != nil {
		return nil, err
	}
	if in.Status < -1 || in.Status > 7 {
		return nil, xerr.Param("批次状态不合法")
	}
	if r := in.DateRange; r != nil && r.Start > 0 && r.End > 0 && r.Start >= r.End {
		return nil, xerr.Param("时间范围不合法")
	}
	page, size := int(in.Page), int(in.PageSize)
	if page < 1 {
		page = 1
	}
	switch {
	case size <= 0:
		size = 20
	case size > 100:
		size = 100
	}
	f := repo.AdminFilter{UserID: in.UserId, Kind: model.Kind(in.Kind), Status: int8(in.Status)}
	if r := in.DateRange; r != nil {
		f.StartMs, f.EndMs = r.Start, r.End
	}
	rp := l.svcCtx.Repo
	list, total, err := rp.Batch.AdminList(l.ctx, f, page, size)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	stats, err := rp.Task.StatsByBatches(l.ctx, ids)
	if err != nil {
		return nil, err
	}
	out := &studio.AdminListBatchesResp{Total: total, List: make([]*studio.AdminBatch, 0, len(list))}
	for _, b := range list {
		out.List = append(out.List, toAdminBatch(b, nil, stats[b.ID])) // 列表不带子任务，封面留空
	}
	return out, nil
}
