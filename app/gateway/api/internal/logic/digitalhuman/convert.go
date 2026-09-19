package digitalhuman

import (
	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/gateway/api/internal/types"
)

func toOptions(list []*digitalhuman.OptionInfo) []types.Option {
	out := make([]types.Option, 0, len(list))
	for _, o := range list {
		out = append(out, types.Option{Code: o.Code, Label: o.Label, Value: o.Value})
	}
	return out
}

func toPortrait(p *digitalhuman.PortraitInfo) *types.Portrait {
	urls := p.ImageUrls
	if urls == nil {
		urls = []string{}
	}
	return &types.Portrait{
		Id: p.Id, Gender: p.Gender, Age: p.Age, SkinTone: p.SkinTone, Role: p.Role, Description: p.Description,
		Status: p.Status, ImageUrls: urls, FailReason: p.FailReason, CreateTime: p.CreateTime,
	}
}

func toDigitalHuman(h *digitalhuman.DigitalHumanInfo) *types.DigitalHuman {
	return &types.DigitalHuman{
		Id: h.Id, Name: h.Name, PortraitSource: h.PortraitSource, ImageUrl: h.ImageUrl, VoiceId: h.VoiceId,
		VoiceName: h.VoiceName, Script: h.Script, VideoUrl: h.VideoUrl, CoverUrl: h.CoverUrl, Duration: h.Duration,
		Status: h.Status, FailReason: h.FailReason, CreateTime: h.CreateTime,
	}
}
