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
		ImageCount: c.ImageCount, ImageUrls: orEmpty(c.ImageUrls), Status: c.Status, FailReason: c.FailReason,
		CreateTime: c.CreateTime,
	}
}
