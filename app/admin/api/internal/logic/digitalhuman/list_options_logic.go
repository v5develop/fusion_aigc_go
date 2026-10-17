// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListOptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 数字人选项列表（年龄/肤色/职业/音色分类/提示词）
func NewListOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListOptionsLogic {
	return &ListOptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListOptionsLogic) ListOptions(req *types.DhOptionListReq) (resp *types.OptionListResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AdminListOptions(l.ctx, &digitalhuman.AdminListOptionsReq{OptionType: req.OptionType})
	if err != nil {
		return nil, err
	}
	resp = &types.OptionListResp{List: make([]types.Option, 0, len(out.List))}
	for _, o := range out.List {
		resp.List = append(resp.List, *toOption(o))
	}
	return resp, nil
}
