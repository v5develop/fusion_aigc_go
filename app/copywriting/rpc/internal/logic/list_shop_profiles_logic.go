package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/pagex"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListShopProfilesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListShopProfilesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListShopProfilesLogic {
	return &ListShopProfilesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListShopProfilesLogic) ListShopProfiles(in *pb.ShopProfileListReq) (*pb.ShopProfileList, error) {
	limit, offset := pagex.Offset(in.Page, in.PageSize)
	rows, total, err := l.svcCtx.ShopProfile.ListByUser(l.ctx, in.UserId, limit, offset)
	if err != nil {
		return nil, err
	}
	resp := &pb.ShopProfileList{Total: total, List: make([]*pb.ShopProfile, 0, len(rows))}
	for _, p := range rows {
		resp.List = append(resp.List, toShopProfile(p))
	}
	return resp, nil
}
