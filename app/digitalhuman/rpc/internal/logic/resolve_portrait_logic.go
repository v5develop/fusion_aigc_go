package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ResolvePortraitLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewResolvePortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResolvePortraitLogic {
	return &ResolvePortraitLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ResolvePortrait 把「形象来源 + id」解析成形象图地址，供动作迁移等模块复用
func (l *ResolvePortraitLogic) ResolvePortrait(in *pb.ResolvePortraitReq) (*pb.PortraitImage, error) {
	src, err := resolvePortraitSource(l.ctx, l.svcCtx, &pb.CreateReq{
		UserId: in.UserId, PortraitSource: in.PortraitSource, ImageUrl: in.ImageUrl,
		PublicId: in.PublicId, PortraitId: in.PortraitId, ImageIndex: in.ImageIndex,
	})
	if err != nil {
		return nil, err
	}
	return &pb.PortraitImage{ImageUrl: src.ImageURL, RefId: src.RefID, DefaultName: src.DefaultName}, nil
}
