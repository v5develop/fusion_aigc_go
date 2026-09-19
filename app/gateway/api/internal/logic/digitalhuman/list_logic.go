// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的数字人列表
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.ListDigitalHumansReq) (resp *types.DigitalHumanListResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.List(l.ctx, &digitalhuman.ListReq{
		UserId: ctxdata.UserID(l.ctx), Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.DigitalHumanListResp{List: make([]types.DigitalHuman, 0, len(out.List)), Total: out.Total}
	for _, h := range out.List {
		resp.List = append(resp.List, *toDigitalHuman(h))
	}
	return resp, nil
}
