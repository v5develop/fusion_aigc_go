package videogen

import (
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
)

func toVideoCreation(c *imagegen.VideoInfo) *types.VideoCreation {
	return &types.VideoCreation{
		Id: c.Id, Kind: c.Kind, UserText: c.UserText, SellingPoints: c.SellingPoints,
		ShopProfileId: c.ShopProfileId, CharUrl: c.CharUrl, Env1Url: c.Env1Url, Env2Url: c.Env2Url,
		PropUrl: c.PropUrl, Duration: c.Duration, Size: c.Size, Model: c.Model, Script: c.Script,
		VideoUrl: c.VideoUrl, Status: c.Status, FailReason: c.FailReason, CreateTime: c.CreateTime,
		Source: c.Source, SourceUrl: c.SourceUrl, SourceVideoUrl: c.SourceVideoUrl, Title: c.Title, CoverUrl: c.CoverUrl,
	}
}

func toOptions(list []*imagegen.Option) []types.ImageGenOption {
	out := make([]types.ImageGenOption, 0, len(list))
	for _, o := range list {
		out = append(out, types.ImageGenOption{Code: o.Code, Label: o.Label})
	}
	return out
}

func orEmptyInt32(v []int32) []int32 {
	if v == nil {
		return []int32{}
	}
	return v
}

func orEmptyStr(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
