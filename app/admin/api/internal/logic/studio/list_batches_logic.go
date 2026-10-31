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

type ListBatchesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批次列表
func NewListBatchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBatchesLogic {
	return &ListBatchesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListBatchesLogic) ListBatches(req *types.StudioListBatchesReq) (resp *types.StudioListBatchesResp, err error) {
	out, err := l.svcCtx.StudioRpc.AdminListBatches(l.ctx, &studio_client.AdminListBatchesReq{
		Page: req.Page, PageSize: req.PageSize, UserId: req.UserId, Kind: req.Kind, Status: req.Status,
		DateRange: &studio_client.DateRange{Start: req.StartAt, End: req.EndAt},
	})
	if err != nil {
		return nil, err
	}
	resp = &types.StudioListBatchesResp{List: make([]types.StudioAdminBatch, 0, len(out.List)), Total: out.Total}
	for _, b := range out.List {
		resp.List = append(resp.List, toAdminBatch(b))
	}
	return resp, nil
}
