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

type ActionVideosLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 动作视频：预置（运营维护）+ 我导入的
func NewActionVideosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ActionVideosLogic {
	return &ActionVideosLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ActionVideosLogic) ActionVideos() (resp *types.ActionVideoListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.ListActionVideos(l.ctx, &imagegen.IdUserReq{UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return &types.ActionVideoListResp{Presets: toActionVideos(out.Presets), Mine: toActionVideos(out.Mine)}, nil
}
