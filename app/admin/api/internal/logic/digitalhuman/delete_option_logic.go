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

type DeleteOptionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除数字人选项
func NewDeleteOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteOptionLogic {
	return &DeleteOptionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteOptionLogic) DeleteOption(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.DigitalHumanRpc.AdminDeleteOption(l.ctx, &digitalhuman.IdReq{Id: req.Id}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
