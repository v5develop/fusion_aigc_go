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

type DeleteOptionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除选项
func NewDeleteOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteOptionLogic {
	return &DeleteOptionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteOptionLogic) DeleteOption(req *types.IdPath) (resp *types.EmptyResp, err error) {
	_, err = l.svcCtx.CopywritingRpc.AdminDeleteOption(l.ctx, &copywriting.IdReq{Id: req.Id})
	return &types.EmptyResp{}, err
}
