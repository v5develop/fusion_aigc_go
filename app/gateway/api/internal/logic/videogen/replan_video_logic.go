// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReplanVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 规划剧本页的「重新生成」：重新构思
func NewReplanVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReplanVideoLogic {
	return &ReplanVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ReplanVideoLogic) ReplanVideo(req *types.IdPath) (resp *types.VideoCreation, err error) {
	out, err := l.svcCtx.ImageGenRpc.ReplanVideo(l.ctx, &imagegen.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return toVideoCreation(out), nil
}
