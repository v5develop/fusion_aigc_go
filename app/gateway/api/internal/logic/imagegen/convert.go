package imagegen

import (
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
)

func toOptions(list []*imagegen.Option) []types.CreationOption {
	out := make([]types.CreationOption, 0, len(list))
	for _, o := range list {
		out = append(out, types.CreationOption{Code: o.Code, Label: o.Label})
	}
	return out
}

func toCreation(c *imagegen.CreationInfo) *types.Creation {
	orEmpty := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	out := &types.Creation{
		Id: c.Id, Kind: c.Kind, Media: c.Media, Content: c.Content, MaterialUrls: orEmpty(c.MaterialUrls), TemplateId: c.TemplateId,
		TemplateUrl: c.TemplateUrl, PosterType: c.PosterType, PosterStyle: c.PosterStyle, Size: c.Size,
		ImageCount: c.ImageCount, ImageUrls: orEmpty(c.ImageUrls), InputImageUrl: c.InputImageUrl,
		InputVideoUrl: c.InputVideoUrl, Model: c.Model, InputBgUrl: c.InputBgUrl, Duration: c.Duration,
		Status: c.Status, FailReason: c.FailReason,
		CreateTime: c.CreateTime,
	}
	// 视频成品（口播、动作迁移）存在图片表里，地址只放 video_url，image_urls 为空
	if c.Media == "video" {
		out.ImageUrls = []string{}
		if len(c.ImageUrls) > 0 {
			out.VideoUrl = c.ImageUrls[0]
		}
	}
	return out
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
