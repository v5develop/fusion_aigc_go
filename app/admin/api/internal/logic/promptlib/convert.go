package promptlib

import (
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/canvas/rpc/canvas_client"
)

func toItem(p *canvas_client.PromptItem) types.PromptLibAdminItem {
	out := types.PromptLibAdminItem{
		Id: p.Id, UserId: p.UserId, Title: p.Title, Prompt: p.Prompt, Vertical: p.Vertical, Group: p.Group, Models: p.Models, Tags: p.Tags,
		Source: p.Source, Owner: p.Owner, Enabled: p.Enabled, Sort: p.Sort, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	return out
}
