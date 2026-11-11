package logic

import (
	"context"
	"encoding/json"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// regenMaxAttempts 编辑提示词重新生成的轮次上限（与用户重试 / 运营重做共用 attempt 计数）。
// 比 userRetryMaxAttempts 宽：改提示词是用户主动迭代，不是对确定性失败的盲目重试；每轮都照常计费。
const regenMaxAttempts = 6

// stepUserRegenerate step_log 步骤名。
const stepUserRegenerate = "user_regenerate"

type RegenerateTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegenerateTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegenerateTaskLogic {
	return &RegenerateTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RegenerateTask 编辑提示词后重新生成单张（姿态复刻，验收 C4 / C17）：用改过的提示词重跑该子任务的生图一步。
//
//   - 只收本人的姿态子任务，失败或成功的都可以（成功的旧成品已在素材库，任务行改由新结果覆盖）；
//   - 计费同用户重试：新号 studio-<task_id>-r<n> 冻结 1 张，与提交同一入口码 / 模型组 / 分辨率（retryCharge）；
//   - 幂等：任务已在一次重做中且提示词与本次相同 → 不再冻结，直接回当前详情；提示词不同 → 冲突（等这张结束再改）。
func (l *RegenerateTaskLogic) RegenerateTask(in *studio.RegenerateTaskReq) (*studio.BatchDetail, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	taskID := strings.TrimSpace(in.TaskId)
	if taskID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	body := strings.TrimSpace(pipeline.StripImageOrderLine(strings.TrimSpace(in.Prompt)))
	if body == "" {
		return nil, xerr.New(xerr.ParamMissing, "请填写提示词")
	}
	if err := checkPromptLen("提示词", body); err != nil {
		return nil, err
	}
	f := newAdminFlow(l.svcCtx)
	return regenerate(l.ctx, f, l.svcCtx.Moderation, in.UserId, taskID, body, func(b *model.Batch, tasks []*model.Task) *studio.BatchDetail {
		out := toBatchDetail(b, tasks)
		attachAssetIDsToDetails(l.ctx, f.repo, in.UserId, out)
		signDetails(l.ctx, l.svcCtx, out)
		return out
	})
}

// regenerate RegenerateTask 的主体（单测直接调）。body 是去掉图序行后的提示词正文。
func regenerate(ctx context.Context, f *flow, mod moderation.Checker, userID int64, taskID, body string,
	render func(*model.Batch, []*model.Task) *studio.BatchDetail) (*studio.BatchDetail, error) {
	t, b, err := ownedTask(ctx, f, taskID, userID)
	if err != nil {
		return nil, err
	}
	if t.Kind != model.KindPose {
		return nil, xerr.Param("只有姿态复刻支持编辑提示词后重新生成")
	}
	text := pipeline.WithImageOrderLine(body, pipeline.KindPose, 2) // 图1 原图，图2 姿势参考图（姿态恒为两张）
	if !(retriedInFlight(t) && t.PromptText.String == text) {
		if err := checkText(ctx, mod, "studio_pose_prompt", body); err != nil {
			return nil, err
		}
		if err := f.userRegenerate(ctx, t, b, text); err != nil {
			if code, _, _ := xerr.Parse(err); code != xerr.Conflict {
				return nil, err
			}
			// 冲突：可能是同一次提交的并发请求先占了轮次 —— 重读，已按同一提示词在重做中就按成功返回
			t2, _, rerr := ownedTask(ctx, f, taskID, userID)
			if rerr != nil || !retriedInFlight(t2) || t2.PromptText.String != text {
				return nil, err
			}
		} else {
			logx.WithContext(ctx).Infow("studio user regenerate", logx.Field("task_id", taskID), logx.Field("attempt", t.Attempt+1), logx.Field("user_id", userID))
		}
	}
	b, err = f.repo.Batch.GetByID(ctx, b.ID, userID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, xerr.NotFoundf("批次")
	}
	tasks, err := f.repo.Task.ListByBatch(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	return render(b, tasks), nil
}

// userRegenerate 前置条件（与 userRetry 同口径，另收 成功 的任务），全部通过后走 retryTaskOpts（占轮次 → 冻结新单 → 换提示词排队 → 批次拉回）：
//   - 失败 / 成功的任务；在途的拒绝（等这张结束再改）；已取消、运营封禁、超过轮次的拒绝；
//   - 原预留单：失败的必须已了结且没扣费（同 userRetry）；成功的原单已结算（扣的是上一张图的钱），只拒绝仍 HELD 的；
//   - 只读余额预检不够直接 3003，不占轮次。
func (f *flow) userRegenerate(ctx context.Context, t *model.Task, b *model.Batch, text string) error {
	if t.AdminLocked != 0 {
		return xerr.New(xerr.Conflict, "该任务已停止重试，请重新提交")
	}
	switch t.Status {
	case model.TaskFailed, model.TaskSucceeded:
	case model.TaskCanceled:
		return xerr.New(xerr.Conflict, "已取消的任务不能重新生成")
	default:
		return xerr.New(xerr.Conflict, "这张图正在生成中，请等结束后再修改提示词")
	}
	switch b.Status {
	case model.BatchDone, model.BatchFailed, model.BatchPartial, model.BatchGenerating:
	case model.BatchCanceled:
		return xerr.New(xerr.Conflict, "批次已取消，不能重新生成")
	default:
		return xerr.New(xerr.Conflict, errBatchRunning)
	}
	if !t.PromptText.Valid || strings.TrimSpace(t.PromptText.String) == "" || len(t.InputImages) == 0 {
		return xerr.New(xerr.Conflict, "这张图还没生成提示词，无法单独重新生成，请重新提交")
	}
	if t.Attempt >= regenMaxAttempts {
		return xerr.New(xerr.Conflict, "这张图已重新生成多次，请调整素材后重新提交")
	}
	// 付费成品还在后台入素材库（mr://，library_copy.go）：等它入完再重做，旧成品照常换成公开地址、作为当前成品入库。
	// 万一竞态漏过这道关（旧成品在重做之后才转存完），旧成品带 supersededSrcRid 标记入素材库、新成品按地址查重，
	// 两张各入一次（见 library_copy.go「成品已换」）；这里拦住只是让常规路径不走那条兜底。
	if t.Status == model.TaskSucceeded && isPendingRef(t.ResultURL.String) && f.repo != nil {
		j, err := f.repo.CopyJob.GetPendingByTask(ctx, t.ID)
		if err != nil && !isMissingTable(err) {
			logx.WithContext(ctx).Errorf("studio: 重新生成子任务 %d 查成品入库状态失败: %v", t.ID, err)
			return xerr.New(xerr.ServerError, "暂时无法重新生成，请稍后重试")
		}
		if j != nil {
			return xerr.New(xerr.Conflict, "这张图正在存入素材库，请几秒后再试")
		}
	}
	if !f.gwOn {
		return xerr.New(xerr.NotReady, "生图服务暂未开放，请稍后再试")
	}
	if t.BillingStatus == model.BillingHeld {
		return xerr.New(xerr.Conflict, "上一次的点数还在结算中，请稍后再试")
	}
	if t.Status == model.TaskFailed && t.BillingStatus == model.BillingSettled {
		return xerr.New(xerr.Conflict, "上一次的点数还在结算中，请稍后再试")
	}
	if b.Status == model.BatchGenerating {
		if err := f.onlyRetriesInFlight(ctx, t, b); err != nil {
			return err
		}
	}
	entry := svc.EntryCode(f.cfg, t.Kind)
	hctx, cancel := detached(ctx)
	defer cancel()
	if f.bill.Mode() == billing.ModeOn {
		if old := t.BillingRequestID.String; old != "" {
			st, err := f.bill.Status(hctx, b.UserID, old)
			if err != nil {
				logx.WithContext(ctx).Errorf("studio: 重新生成子任务 %d 查原预留单 %s 失败: %v", t.ID, old, err)
				return xerr.New(xerr.ServerError, "暂时无法核对计费，请稍后重试")
			}
			done := st == "" || st == resvRefunded || st == resvExpired
			if t.Status == model.TaskSucceeded {
				done = done || st == resvConfirmed // 成功的图原单已扣费（那张图的钱），不再动它
			}
			if !done {
				logx.WithContext(ctx).Errorf("studio: 重新生成子任务 %d 拒绝：原预留单 %s 状态 %s", t.ID, old, st)
				return xerr.New(xerr.Conflict, "上一次的点数还在结算中，请稍后再试")
			}
		}
	}
	if err := f.bill.CheckAccess(hctx, b.UserID, entry); err != nil {
		return accessError(ctx, b.UserID, entry, err)
	}
	charge, err := f.retryCharge(t, b)
	if err != nil {
		return err
	}
	if err := f.bill.Enough(hctx, charge); err != nil {
		return err
	}
	meta, err := f.regenMeta(ctx, t, b)
	if err != nil {
		return err
	}
	return f.retryTaskOpts(ctx, t, b, stepUserRegenerate, retryOpts{Prompt: text, Meta: meta, FromSucceeded: true})
}

// regenMeta 重新生成后的 prompt_meta：原 meta 保留（生图参数 gen 必须沿用），记 edited；从成功重做时记下当前素材库里该任务
// 最新资产 id（regen_asset_floor），交付时 saveAsset 只把比它新的资产当作「已入库」，新成品才能照常入库、重放也不会写两条。
func (f *flow) regenMeta(ctx context.Context, t *model.Task, b *model.Batch) (json.RawMessage, error) {
	m := map[string]any{}
	if len(t.PromptMeta) > 0 {
		_ = json.Unmarshal(t.PromptMeta, &m)
	}
	m["edited"] = true
	delete(m, metaUserWarnings) // 原提示词的读图 / 配对提示不再适用
	if t.Status == model.TaskSucceeded {
		ids, err := f.repo.Task.AssetIDs(ctx, b.UserID, []string{t.TaskID.String})
		if err != nil {
			return nil, xerr.New(xerr.ServerError, "暂时无法重新生成，请稍后重试")
		}
		m[metaRegenAssetFloor] = ids[t.TaskID.String]
	}
	return json.Marshal(m)
}

// regenFloorSet prompt_meta 里是否有 regen_asset_floor 键（= 从成功重新生成过；值可能为 0：重做时旧成品还没入素材库）。
// 有这个键的子任务入库按成品地址查重（见 saveAssetErr）。
func regenFloorSet(t *model.Task) bool {
	if len(t.PromptMeta) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(t.PromptMeta, &m) != nil {
		return false
	}
	_, ok := m[metaRegenAssetFloor]
	return ok
}

// metaRegenAssetFloor 见 regenMeta。
const metaRegenAssetFloor = "regen_asset_floor"

// regenAssetFloor 子任务 prompt_meta.regen_asset_floor（没有为 0）。
func regenAssetFloor(t *model.Task) int64 {
	if len(t.PromptMeta) == 0 {
		return 0
	}
	var m struct {
		Floor int64 `json:"regen_asset_floor"`
	}
	_ = json.Unmarshal(t.PromptMeta, &m)
	return m.Floor
}
