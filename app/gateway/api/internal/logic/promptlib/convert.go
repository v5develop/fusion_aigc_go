package promptlib

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/types"
)

func toItem(p *canvas_client.PromptItem) types.PromptLibItem {
	out := types.PromptLibItem{
		Id: p.Id, Title: p.Title, Prompt: p.Prompt, Vertical: p.Vertical, Group: p.Group, Models: p.Models, Tags: p.Tags,
		Source: p.Source, Owner: p.Owner, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	return out
}
