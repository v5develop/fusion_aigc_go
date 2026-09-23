// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type VideoOptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 表单配置（模型/时长/比例/字数上限）
func NewVideoOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VideoOptionsLogic {
	return &VideoOptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *VideoOptionsLogic) VideoOptions(req *types.VideoOptionsReq) (resp *types.VideoOptionsResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.VideoOptions(l.ctx, &imagegen.KindReq{Kind: req.Kind})
	if err != nil {
		return nil, err
	}
	return &types.VideoOptionsResp{
		Models: toOptions(out.Models), DefaultModel: out.DefaultModel,
		Durations: orEmptyInt32(out.Durations), DefaultDuration: out.DefaultDuration,
		Sizes: toOptions(out.Sizes), DefaultSize: out.DefaultSize,
		MaxUserTextLen: out.MaxUserTextLen, MaxSellingPointsLen: out.MaxSellingPointsLen,
		PropRequired: out.PropRequired,
	}, nil
}
