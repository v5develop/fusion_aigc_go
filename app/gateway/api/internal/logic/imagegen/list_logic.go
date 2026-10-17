// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 列表
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.ListCreationReq) (resp *types.CreationListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.List(l.ctx, &imagegen.ListReq{
		UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.CreationListResp{List: make([]types.Creation, 0, len(out.List)), Total: out.Total}
	for _, c := range out.List {
		resp.List = append(resp.List, *toCreation(c))
	}
	return resp, nil
}
