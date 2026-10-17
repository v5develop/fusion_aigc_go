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

func (l *ListLogic) List(req *types.CreationListReq) (resp *types.CreationListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.AdminList(l.ctx, &imagegen.ListReq{
		UserId: req.UserId, Kind: req.Kind, Media: req.Media, Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.CreationListResp{List: make([]types.CreationItem, 0, len(out.List)), Total: out.Total}
	for _, c := range out.List {
		item := types.CreationItem{
			Id: c.Id, UserId: c.UserId, Kind: c.Kind, Media: c.Media, Content: c.Content, ImageUrls: []string{},
			Status: c.Status, FailReason: c.FailReason, CreateTime: c.CreateTime,
		}
		// 视频成品（动作迁移、数字人口播）存在图片表里，地址只放 video_url，不再混进 image_urls
		if c.Media == "video" {
			if len(c.ImageUrls) > 0 {
				item.VideoUrl = c.ImageUrls[0]
			}
		} else if c.ImageUrls != nil {
			item.ImageUrls = c.ImageUrls
		}
		resp.List = append(resp.List, item)
	}
	return resp, nil
}
