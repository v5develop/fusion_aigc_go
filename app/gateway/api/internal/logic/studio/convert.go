package studio

import (
	"strconv"
	"strings"

	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/xerr"
)

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func toSubmitResp(r *studio_client.SubmitResp) *types.StudioSubmitResp {
	return &types.StudioSubmitResp{
		BatchId: r.BatchId, TaskTotal: r.TaskTotal, EstimatedPoints: r.EstimatedPoints,
		TaskIds: orEmpty(r.TaskIds), Replayed: r.Replayed, Warnings: orEmpty(r.Warnings),
	}
}

func toBatch(b *studio_client.BatchInfo) types.StudioBatch {
	if b == nil {
		return types.StudioBatch{}
	}
	return types.StudioBatch{
		Id: b.Id, Kind: b.Kind, Status: b.Status, Payload: b.Payload,
		TaskTotal: b.TaskTotal, TaskDone: b.TaskDone, TaskFailed: b.TaskFailed, EstimatedPoints: b.EstimatedPoints,
		ErrorReason: b.ErrorReason, CoverUrl: b.CoverUrl, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

func toTask(t *studio_client.TaskInfo) types.StudioTask {
	if t == nil {
		return types.StudioTask{}
	}
	return types.StudioTask{
		Id: t.Id, TaskId: t.TaskId, BatchId: t.BatchId, Kind: t.Kind,
		ModelIndex: t.ModelIndex, ShotIndex: t.ShotIndex, PoseIndex: t.PoseIndex,
		Status: t.Status, Progress: t.Progress, ResultUrl: t.ResultUrl, ResultWidth: t.ResultWidth, ResultHeight: t.ResultHeight,
		ErrorReason: t.ErrorReason, PromptSummary: t.PromptSummary,
		CreatedAt: t.CreatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
	}
}

func toDetail(d *studio_client.BatchDetail) types.StudioBatchDetail {
	out := types.StudioBatchDetail{Batch: toBatch(d.GetBatch()), Tasks: make([]types.StudioTask, 0, len(d.GetTasks()))}
	for _, t := range d.GetTasks() {
		out.Tasks = append(out.Tasks, toTask(t))
	}
	return out
}

// posePrompts HTTP 的 {"0": "…"} → rpc 的 {0: "…"}。键必须是非负整数，范围由 rpc 校验。
func posePrompts(m map[string]string) (map[int32]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[int32]string, len(m))
	for k, v := range m {
		n, err := strconv.ParseInt(strings.TrimSpace(k), 10, 32)
		if err != nil || n < 0 {
			return nil, xerr.Param("pose_prompts 的键应为姿势下标（从 0 起）：" + k)
		}
		out[int32(n)] = v
	}
	return out, nil
}
