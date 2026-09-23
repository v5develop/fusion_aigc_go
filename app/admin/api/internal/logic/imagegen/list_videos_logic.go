// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListVideosLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户探店视频/带货短剧列表（只读，排查用）
func NewListVideosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListVideosLogic {
	return &ListVideosLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListVideosLogic) ListVideos(req *types.AdminVideoListReq) (resp *types.AdminVideoListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.AdminListVideos(l.ctx, &imagegen.VideoListReq{
		UserId: req.UserId, Kind: req.Kind, Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.AdminVideoListResp{Total: out.Total, List: make([]types.AdminVideoCreation, 0, len(out.List))}
	for _, c := range out.List {
		resp.List = append(resp.List, types.AdminVideoCreation{
			Id: c.Id, UserId: c.UserId, Kind: c.Kind, UserText: c.UserText, SellingPoints: c.SellingPoints,
			Duration: c.Duration, Size: c.Size, Model: c.Model, VideoUrl: c.VideoUrl,
			Status: c.Status, FailReason: c.FailReason, CreateTime: c.CreateTime,
		})
	}
	return resp, nil
}
