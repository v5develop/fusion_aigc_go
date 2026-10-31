package studio

import (
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
)

func toAdminBatch(a *studio_client.AdminBatch) types.StudioAdminBatch {
	if a == nil {
		return types.StudioAdminBatch{}
	}
	b := a.GetBatch()
	return types.StudioAdminBatch{
		Id: b.GetId(), UserId: a.UserId, WorkspaceId: a.WorkspaceId, Kind: b.GetKind(), Status: b.GetStatus(),
		RequestId: b.GetRequestId(), Payload: b.GetPayload(), TaskTotal: b.GetTaskTotal(), TaskDone: b.GetTaskDone(),
		TaskFailed: b.GetTaskFailed(), EstimatedPoints: b.GetEstimatedPoints(), CostCny: a.CostCny, ModelGroup: a.ModelGroup,
		ErrorReason: b.GetErrorReason(), CoverUrl: b.GetCoverUrl(), CreatedAt: b.GetCreatedAt(), UpdatedAt: b.GetUpdatedAt(),
	}
}

func toAdminTask(a *studio_client.AdminTask) types.StudioAdminTask {
	t := a.GetTask()
	return types.StudioAdminTask{
		Id: t.GetId(), TaskId: t.GetTaskId(), ModelIndex: t.GetModelIndex(), ShotIndex: t.GetShotIndex(), PoseIndex: t.GetPoseIndex(),
		Status: t.GetStatus(), ResultUrl: t.GetResultUrl(), ResultWidth: t.GetResultWidth(), ResultHeight: t.GetResultHeight(),
		ErrorReason: t.GetErrorReason(), ModelGroup: t.GetModelGroup(),
		PromptText: a.PromptText, PromptMeta: a.PromptMeta, InputImages: a.InputImages,
		GatewayCallId: a.GatewayCallId, GatewayStatus: a.GatewayStatus, Attempt: a.Attempt, AdminLocked: a.AdminLocked,
		CostCny: a.CostCny, BillingRequestId: a.BillingRequestId, BillingStatus: a.BillingStatus, DurationMs: a.DurationMs,
		CreatedAt: t.GetCreatedAt(), StartedAt: t.GetStartedAt(), FinishedAt: t.GetFinishedAt(),
	}
}

func toCostRow(r *studio_client.CostRow) types.StudioCostRow {
	if r == nil {
		return types.StudioCostRow{CostCny: "0"}
	}
	return types.StudioCostRow{
		Key: r.Key, TaskTotal: r.TaskTotal, TaskSucceeded: r.TaskSucceeded, TaskFailed: r.TaskFailed,
		CostCny: r.CostCny, Points: r.Points,
	}
}
