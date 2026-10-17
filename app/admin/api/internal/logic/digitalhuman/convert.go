package digitalhuman

import (
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
)

func toOption(o *digitalhuman.OptionInfo) *types.Option {
	return &types.Option{
		Id: o.Id, OptionType: o.OptionType, Code: o.Code, Label: o.Label,
		Value: o.Value, SortOrder: o.SortOrder, Status: o.Status,
	}
}

func fromSaveOption(req *types.DhSaveOptionReq) *digitalhuman.OptionInfo {
	return &digitalhuman.OptionInfo{
		Id: req.Id, OptionType: req.OptionType, Code: req.Code, Label: req.Label,
		Value: req.Value, SortOrder: req.SortOrder, Status: req.Status,
	}
}

func toVoice(v *digitalhuman.VoiceInfo) *types.Voice {
	cats := v.Categories
	if cats == nil {
		cats = []string{}
	}
	return &types.Voice{
		Id: v.Id, Name: v.Name, Voice: v.Voice, Gender: v.Gender, Style: v.Style, Categories: cats,
		PreviewUrl: v.PreviewUrl, SortOrder: v.SortOrder, Status: v.Status,
	}
}

func fromSaveVoice(req *types.SaveVoiceReq) *digitalhuman.VoiceInfo {
	return &digitalhuman.VoiceInfo{
		Id: req.Id, Name: req.Name, Voice: req.Voice, Gender: req.Gender, Style: req.Style,
		Categories: req.Categories, PreviewUrl: req.PreviewUrl, SortOrder: req.SortOrder, Status: req.Status,
	}
}
