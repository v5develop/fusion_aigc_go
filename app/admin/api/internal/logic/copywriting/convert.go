package copywriting

import (
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/copywriting/rpc/copywriting"
)

func toOption(o *copywriting.OptionInfo) *types.Option {
	return &types.Option{
		Id: o.Id, OptionType: o.OptionType, Code: o.Code, Label: o.Label,
		Value: o.Value, SortOrder: o.SortOrder, Status: o.Status,
	}
}

func fromSaveOption(req *types.SaveOptionReq) *copywriting.OptionInfo {
	return &copywriting.OptionInfo{
		Id: req.Id, OptionType: req.OptionType, Code: req.Code, Label: req.Label,
		Value: req.Value, SortOrder: req.SortOrder, Status: req.Status,
	}
}
