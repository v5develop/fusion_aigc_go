// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"aigc-life/app/copywriting/rpc/copywriting"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListOptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 选项列表
func NewListOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListOptionsLogic {
	return &ListOptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListOptionsLogic) ListOptions(req *types.OptionListReq) (resp *types.OptionListResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.AdminListOptions(l.ctx, &copywriting.AdminListOptionsReq{OptionType: req.OptionType})
	if err != nil {
		return nil, err
	}
	resp = &types.OptionListResp{List: make([]types.Option, 0, len(out.List))}
	for _, o := range out.List {
		resp.List = append(resp.List, *toOption(o))
	}
	return resp, nil
}
