// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPublicLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 公共数字人列表
func NewListPublicLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPublicLogic {
	return &ListPublicLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListPublicLogic) ListPublic() (resp *types.PublicListResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.ListPublic(l.ctx, &digitalhuman.Empty{})
	if err != nil {
		return nil, err
	}
	resp = &types.PublicListResp{List: make([]types.PublicDigitalHuman, 0, len(out.List))}
	for _, p := range out.List {
		resp.List = append(resp.List, types.PublicDigitalHuman{Id: p.Id, Name: p.Name, RoleLabel: p.RoleLabel, ImageUrl: p.ImageUrl, AvatarUrl: p.AvatarUrl})
	}
	return resp, nil
}
