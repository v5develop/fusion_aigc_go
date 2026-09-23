// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListVideosLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 列表
func NewListVideosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListVideosLogic {
	return &ListVideosLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListVideosLogic) ListVideos(req *types.ListVideoCreationsReq) (resp *types.VideoCreationListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.ListVideos(l.ctx, &imagegen.VideoListReq{
		UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.VideoCreationListResp{Total: out.Total, List: make([]types.VideoCreation, 0, len(out.List))}
	for _, c := range out.List {
		resp.List = append(resp.List, *toVideoCreation(c))
	}
	return resp, nil
}
