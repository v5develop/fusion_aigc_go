// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户海报/长图列表（只读，排查用）
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.ImageGenListReq) (resp *types.ImageGenListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.AdminList(l.ctx, &imagegen.ListReq{
		UserId: req.UserId, Kind: req.Kind, Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.ImageGenListResp{List: make([]types.ImageCreation, 0, len(out.List)), Total: out.Total}
	for _, c := range out.List {
		urls := c.ImageUrls
		if urls == nil {
			urls = []string{}
		}
		resp.List = append(resp.List, types.ImageCreation{
			Id: c.Id, UserId: c.UserId, Kind: c.Kind, Content: c.Content, ImageUrls: urls,
			Status: c.Status, FailReason: c.FailReason, CreateTime: c.CreateTime,
		})
	}
	return resp, nil
}
