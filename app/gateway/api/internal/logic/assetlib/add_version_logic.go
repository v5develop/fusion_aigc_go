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

type AddVersionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 追加版本（版本号自增），并设为当前版本
func NewAddVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddVersionLogic {
	return &AddVersionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *AddVersionLogic) AddVersion(req *types.AssetLibAddVersionReq) (resp *types.AssetLibVersion, err error) {
	out, err := l.svcCtx.AssetLibRpc.AddVersion(l.ctx, &assetlib_client.AddVersionReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx),
		ImageUrl: req.ImageUrl, Prompt: req.Prompt, Model: req.Model, Params: req.Params, ImageRid: req.ImageRid})
	if err != nil {
		return nil, err
	}
	v := toVersion(out)
	return &v, nil
}
