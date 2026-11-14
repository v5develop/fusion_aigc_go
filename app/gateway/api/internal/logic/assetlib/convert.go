package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"aigc-local-life/app/gateway/api/internal/types"
)

func toVersion(v *assetlib_client.Version) types.AssetLibVersion {
	return types.AssetLibVersion{Id: v.Id, VersionNo: v.VersionNo, Image: v.Image, Prompt: v.Prompt, Model: v.Model,
		Params: v.Params, CreatedAt: v.CreatedAt}
}

func toVersions(vs []*assetlib_client.Version) []types.AssetLibVersion {
	out := make([]types.AssetLibVersion, 0, len(vs))
	for _, v := range vs {
		out = append(out, toVersion(v))
	}
	return out
}

func toAsset(a *assetlib_client.Asset) types.AssetLibAsset {
	out := types.AssetLibAsset{
		Id: a.Id, Kind: a.Kind, Title: a.Title, Prompt: a.Prompt, Image: a.Image, Media: a.Media, VideoUrl: a.VideoUrl,
		Source: a.Source, Tags: a.Tags, CreatedAt: a.CreatedAt, Favorite: a.Favorite, License: a.License,
		Versions: toVersions(a.Versions), CurrentVersionId: a.CurrentVersionId, RelatedIds: a.RelatedIds,
		EntryCode: a.EntryCode, Width: a.Width, Height: a.Height, KindSource: a.KindSource, SysTags: a.SysTags,
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.SysTags == nil {
		out.SysTags = []string{}
	}
	if out.RelatedIds == nil {
		out.RelatedIds = []int64{}
	}
	if m := a.Metadata; m != nil {
		out.Metadata = types.AssetLibMetadata{Industry: m.Industry, Style: m.Style, Shot: m.Shot, Color: m.Color}
	}
	if p := a.Provenance; p != nil {
		out.Provenance = types.AssetLibProvenance{RunId: p.RunId, WorkflowId: p.WorkflowId, NodeId: p.NodeId, Operator: p.Operator,
			Model: p.Model, Params: p.Params, CreatedAt: p.CreatedAt}
	}
	return out
}

func toAssets(list []*assetlib_client.Asset) []types.AssetLibAsset {
	out := make([]types.AssetLibAsset, 0, len(list))
	for _, a := range list {
		out = append(out, toAsset(a))
	}
	return out
}

func toPage(p *assetlib_client.AssetPage) types.AssetLibAssetPage {
	if p == nil {
		return types.AssetLibAssetPage{Items: []types.AssetLibAsset{}}
	}
	return types.AssetLibAssetPage{Items: toAssets(p.Items), Total: p.Total}
}

func toTag(t *assetlib_client.Tag) types.AssetLibTag {
	return types.AssetLibTag{Id: t.Id, Name: t.Name, AssetCount: t.AssetCount, CreatedAt: t.CreatedAt}
}
