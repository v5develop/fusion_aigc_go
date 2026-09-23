package shopprofile

import (
	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/gateway/api/internal/types"
)

func toShopProfile(p *copywriting.ShopProfile) *types.ShopProfile {
	return &types.ShopProfile{
		Id: p.Id, Name: p.Name, OpenDate: p.OpenDate, Address: p.Address, Products: p.Products,
		SellingPoints: p.SellingPoints, SelfTitle: p.SelfTitle, TargetAudience: p.TargetAudience,
		CreateTime: p.CreateTime, UpdateTime: p.UpdateTime,
	}
}
