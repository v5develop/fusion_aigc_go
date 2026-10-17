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

type RemoveActionVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除我导入的动作视频
func NewRemoveActionVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemoveActionVideoLogic {
	return &RemoveActionVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RemoveActionVideoLogic) RemoveActionVideo(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.ImageGenRpc.DeleteActionVideo(l.ctx, &imagegen.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
