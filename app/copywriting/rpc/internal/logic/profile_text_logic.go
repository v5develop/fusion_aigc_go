package logic

import (
	"context"

	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ProfileTextLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewProfileTextLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ProfileTextLogic {
	return &ProfileTextLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ProfileText 档案内容拼成一段文本，给各模块塞进执行面的 shop_profile 参数；id 为 0 或档案已删返回空串
func (l *ProfileTextLogic) ProfileText(in *pb.IdUserReq) (*pb.TextResp, error) {
	return &pb.TextResp{Text: shopProfileTextOf(l.ctx, l.svcCtx, in.Id, in.UserId)}, nil
}
