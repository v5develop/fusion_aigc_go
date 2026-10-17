// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteCopywritingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除用户文案
func NewDeleteCopywritingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteCopywritingLogic {
	return &DeleteCopywritingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteCopywritingLogic) DeleteCopywriting(req *types.IdPath) (resp *types.EmptyResp, err error) {
	l.Infof("admin %d delete copywriting %d", ctxdata.AdminID(l.ctx), req.Id)
	_, err = l.svcCtx.CopywritingRpc.AdminDeleteCopywriting(l.ctx, &copywriting.IdReq{Id: req.Id})
	return &types.EmptyResp{}, err
}
