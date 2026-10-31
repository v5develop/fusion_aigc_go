// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RenameTagLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重命名标签
func NewRenameTagLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenameTagLogic {
	return &RenameTagLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RenameTagLogic) RenameTag(req *types.AssetLibTagRenameReq) (resp *types.AssetLibTag, err error) {
	out, err := l.svcCtx.AssetLibRpc.RenameTag(l.ctx, &assetlib_client.RenameTagReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id, Name: req.Name})
	if err != nil {
		return nil, err
	}
	t := toTag(out)
	return &t, nil
}
