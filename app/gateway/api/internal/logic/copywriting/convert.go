package copywriting

import (
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/gateway/api/internal/types"
)

func toCopywriting(c *copywriting.CopywritingInfo) *types.Copywriting {
	return &types.Copywriting{
		Id: c.Id, Source: c.Source, ContentType: c.ContentType, ContentTypeLabel: c.ContentTypeLabel,
		Style: c.Style, Industry: c.Industry, Title: c.Title, Body: c.Body, WordCount: c.WordCount,
		Topic: c.Topic, TargetWords: c.TargetWords, Status: c.Status, FailReason: c.FailReason,
		CreateTime: c.CreateTime,
	}
}

func toOptions(list []*copywriting.OptionInfo) []types.Option {
	out := make([]types.Option, 0, len(list))
	for _, o := range list {
		out = append(out, types.Option{Code: o.Code, Label: o.Label, Value: o.Value})
	}
	return out
}
