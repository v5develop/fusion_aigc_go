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

type GetVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 详情（构思中、生成中都轮询这个）
func NewGetVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetVideoLogic {
	return &GetVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetVideoLogic) GetVideo(req *types.IdPath) (resp *types.VideoCreation, err error) {
	out, err := l.svcCtx.ImageGenRpc.GetVideo(l.ctx, &imagegen.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return toVideoCreation(out), nil
}
