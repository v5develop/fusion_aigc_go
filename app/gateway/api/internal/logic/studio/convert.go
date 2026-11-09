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

// toQuoteResp 提交前报价。任务数规则与提交拆任务一致：商拍 = 模特图数 × 每位张数，姿态 = 姿势图数，图片生成 = count。
func toQuoteResp(r *studio_client.QuoteResp) *types.StudioQuoteResp {
	return &types.StudioQuoteResp{
		Available: r.Available, Points: r.Points, UnitPoints: r.UnitPoints, PriceUnit: r.PriceUnit,
		TaskTotal: r.TaskTotal, Resolution: r.Resolution, BillingMode: r.BillingMode, Charged: r.Charged,
		Reason: r.Reason, Warnings: orEmpty(r.Warnings),
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
		AssetId: t.AssetId, ColorFixParams: toColorFix(t.ColorFixParams),
	}
}

// toColorFix 已保存的校色参数；没保存过为 nil（JSON null）。
func toColorFix(p *studio_client.ColorFixParams) *types.StudioColorFixParams {
	if p == nil {
		return nil
	}
	return &types.StudioColorFixParams{Hue: p.Hue, Saturation: p.Saturation, Value: p.Value, Brightness: p.Brightness, Contrast: p.Contrast}
}

func toDetail(d *studio_client.BatchDetail) types.StudioBatchDetail {
	out := types.StudioBatchDetail{Batch: toBatch(d.GetBatch()), Tasks: make([]types.StudioTask, 0, len(d.GetTasks()))}
	for _, t := range d.GetTasks() {
		out.Tasks = append(out.Tasks, toTask(t))
	}
	return out
}

// posePrompts HTTP 的 {"0": "…"} → rpc 的 {0: "…"}。键必须是非负整数，范围由 rpc 校验。
// toPosePreviewResp 人物配对预览。数组一律回 []（前端不用判 null）；失败的姿势图 pose 为 null。
func toPosePreviewResp(r *studio_client.PreviewPoseResp) *types.StudioPosePreviewResp {
	out := &types.StudioPosePreviewResp{Poses: []types.StudioPosePreviewItem{}, MappingEnabled: r.MappingEnabled}
	if r.Source != nil {
		out.Source = toPosePreviewImage(r.Source)
	} else {
		out.Source.People = []types.StudioPosePreviewPerson{}
	}
	for _, it := range r.Poses {
		if it == nil {
			continue
		}
		item := types.StudioPosePreviewItem{PoseIndex: it.PoseIndex, DefaultMapping: orEmptyInt32(it.DefaultMapping), Error: it.Error}
		if it.Pose != nil {
			img := toPosePreviewImage(it.Pose)
			item.Pose = &img
		}
		out.Poses = append(out.Poses, item)
	}
	return out
}

func toPosePreviewImage(r *studio_client.PosePreviewImage) types.StudioPosePreviewImage {
	img := types.StudioPosePreviewImage{
		PersonCount: r.PersonCount, Group: r.Group, Interaction: r.Interaction, PeopleTruncated: r.PeopleTruncated,
		People: make([]types.StudioPosePreviewPerson, 0, len(r.People)),
	}
	for _, p := range r.People {
		if p == nil {
			continue
		}
		img.People = append(img.People, types.StudioPosePreviewPerson{
			Label: p.Label, Position: p.Position, Primary: p.Primary, GenderAge: p.GenderAge, Summary: p.Summary,
		})
	}
	return img
}

// poseMappings 提交体里的多人配对 → rpc。
func poseMappings(list []types.StudioPoseMapping) []*studio_client.PoseMapping {
	if len(list) == 0 {
		return nil
	}
	out := make([]*studio_client.PoseMapping, 0, len(list))
	for _, m := range list {
		out = append(out, &studio_client.PoseMapping{PoseIndex: m.PoseIndex, Mapping: m.Mapping, PosePersonCount: m.PosePersonCount})
	}
	return out
}

func orEmptyInt32(v []int32) []int32 {
	if v == nil {
		return []int32{}
	}
	return v
}

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
