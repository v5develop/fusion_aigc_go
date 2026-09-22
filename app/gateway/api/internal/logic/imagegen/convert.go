package imagegen

import (
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"
)

func toOptions(list []*imagegen.Option) []types.ImageGenOption {
	out := make([]types.ImageGenOption, 0, len(list))
	for _, o := range list {
		out = append(out, types.ImageGenOption{Code: o.Code, Label: o.Label})
	}
	return out
}

func toImageCreation(c *imagegen.ImageInfo) *types.ImageCreation {
	orEmpty := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	return &types.ImageCreation{
		Id: c.Id, Kind: c.Kind, Content: c.Content, MaterialUrls: orEmpty(c.MaterialUrls), TemplateId: c.TemplateId,
		TemplateUrl: c.TemplateUrl, PosterType: c.PosterType, PosterStyle: c.PosterStyle, Size: c.Size,
		ImageCount: c.ImageCount, ImageUrls: orEmpty(c.ImageUrls), InputImageUrl: c.InputImageUrl,
		InputVideoUrl: c.InputVideoUrl, Model: c.Model, InputBgUrl: c.InputBgUrl, Duration: c.Duration,
		Status: c.Status, FailReason: c.FailReason,
		CreateTime: c.CreateTime,
	}
}

func orEmptyInt(v []int32) []int32 {
	if v == nil {
		return []int32{}
	}
	return v
}

func toActionVideos(list []*imagegen.ActionVideo) []types.ActionVideo {
	out := make([]types.ActionVideo, 0, len(list))
	for _, v := range list {
		out = append(out, types.ActionVideo{Id: v.Id, Name: v.Name, Url: v.Url, CoverUrl: v.CoverUrl, Mine: v.Mine})
	}
	return out
}
