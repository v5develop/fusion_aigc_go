package canvas

import (
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/canvas/rpc/canvas_client"
)

func toTemplate(t *canvas_client.TemplateInfo) types.CanvasAdminTemplate {
	return types.CanvasAdminTemplate{
		Id: t.Id, Scope: t.Scope, UserId: t.UserId, Title: t.Title, Description: t.Description, CoverUrl: t.CoverUrl,
		Sort: t.Sort, Enabled: t.Enabled, NodeCount: t.NodeCount, EdgeCount: t.EdgeCount, Snapshot: t.Snapshot,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toRun(a *canvas_client.AdminRun) types.CanvasAdminRun {
	r := a.Run
	out := types.CanvasAdminRun{
		Id: a.Id, RunId: r.RunId, TaskId: r.TaskId, UserId: a.UserId, CanvasId: r.CanvasId, NodeId: r.NodeId, Kind: r.Kind,
		Status: r.Status, Progress: r.Progress, Model: r.Model, ModelGroup: a.ModelGroup, Backend: a.Backend, BackendRef: a.BackendRef,
		BillingRequestId: a.BillingRequestId, BillingStatus: a.BillingStatus, Credits: r.Credits, CostCny: a.CostCny, Attempt: a.Attempt,
		VideoUrl: r.VideoUrl, Text: r.Text, Error: r.Error, ErrorCode: r.ErrorCode, Request: a.Request,
		CreatedAt: r.CreatedAt, StartedAt: a.StartedAt, FinishedAt: r.FinishedAt, Candidates: []string{},
	}
	for _, c := range r.Candidates {
		out.Candidates = append(out.Candidates, c.Url)
	}
	return out
}
