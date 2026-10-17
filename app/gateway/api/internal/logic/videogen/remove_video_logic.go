// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type RemoveVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除
func NewRemoveVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemoveVideoLogic {
	return &RemoveVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RemoveVideoLogic) RemoveVideo(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err = l.svcCtx.ImageGenRpc.DeleteVideo(l.ctx, &imagegen.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
