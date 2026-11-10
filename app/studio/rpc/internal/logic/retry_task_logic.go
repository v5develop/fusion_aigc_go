package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// userRetryMaxAttempts 同一个子任务最多重做几轮（attempt 计数，含运营重做）：到上限后只能重新提交。
// 防止确定性失败（审核不过 / 输入图被拒）被反复重试、反复冻结点数。
const userRetryMaxAttempts = 3

type RetryTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryTaskLogic {
	return &RetryTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RetryTask 用户重试单个失败子任务：只重跑该任务的生图一步（沿用已存的 prompt_text / input_images / prompt_meta.gen，
// 不再切图、不再反推），按原入口重新冻结 1 张的点数。返回批次详情（同 GetBatch），前端接着轮询批次即可看到 生成中 → 成功。
//
// 幂等：重复点击时，第二次读到任务已在途且已是重做轮次（计费请求号带 -r<n>）→ 不再冻结，直接回当前详情；
// 两次读到同一快照时只有一方能 CAS 占到轮次，另一方冲突后重读，同样回当前详情。
func (l *RetryTaskLogic) RetryTask(in *studio.RetryTaskReq) (*studio.BatchDetail, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	taskID := strings.TrimSpace(in.TaskId)
	if taskID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	f := newAdminFlow(l.svcCtx) // 不同步任务中心（交给 Poller 下一轮），接口快速返回
	t, b, err := ownedTask(l.ctx, f, taskID, in.UserId)
	if err != nil {
		return nil, err
	}
	if !retriedInFlight(t) {
		if err := f.userRetry(l.ctx, t, b); err != nil {
			if code, _, _ := xerr.Parse(err); code != xerr.Conflict {
				return nil, err
			}
			// 冲突：可能是并发的另一次重试先占了轮次 —— 重读，已在重做中就按成功返回
			if t2, _, rerr := ownedTask(l.ctx, f, taskID, in.UserId); rerr != nil || !retriedInFlight(t2) {
				return nil, err
			}
		} else {
			l.Infow("studio user retry", logx.Field("task_id", taskID), logx.Field("attempt", t.Attempt+1), logx.Field("user_id", in.UserId))
		}
	}
	b, err = f.repo.Batch.GetByID(l.ctx, b.ID, in.UserId)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, xerr.NotFoundf("批次")
	}
	tasks, err := f.repo.Task.ListByBatch(l.ctx, b.ID)
	if err != nil {
		return nil, err
	}
	out := toBatchDetail(b, tasks)
	attachAssetIDsToDetails(l.ctx, f.repo, in.UserId, out)
	signDetails(l.ctx, l.svcCtx, out)
	return out, nil
}

// ownedTask 用户侧取子任务与批次：不存在 / 不是本人 / 已删除一律 NotFound（不暴露别人的任务是否存在）。
func ownedTask(ctx context.Context, f *flow, taskID string, userID int64) (*model.Task, *model.Batch, error) {
	t, err := f.repo.Task.GetByTaskID(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	if t == nil || t.UserID != userID || t.Deleted != 0 {
		return nil, nil, xerr.NotFoundf("任务")
	}
	b, err := f.repo.Batch.GetByID(ctx, t.BatchID, userID)
	if err != nil {
		return nil, nil, err
	}
	if b == nil {
		return nil, nil, xerr.NotFoundf("任务")
	}
	return t, b, nil
}

// retriedInFlight 子任务已在一次重做中（在途且计费请求号是重做轮次）：重复的重试请求按幂等成功处理。
func retriedInFlight(t *model.Task) bool {
	return model.TaskActive(t.Status) && retryRound(t.BillingRequestID.String) > 0
}

// userRetry 用户重试的前置条件，全部通过后走与运营重做同一条 retryTask（占轮次 → 冻结新单 → 排队 → 批次拉回）：
//   - 只收 失败 的子任务（已取消的是用户自己取消的，不提供重试）、未被运营封禁、未超过重做轮次上限；
//   - 批次是 全部失败 / 部分成功，或 生图中但在途的只有重做轮次（同批另一张正在重试，见 onlyRetriesInFlight）；原始流程还在跑（编排中 / 首轮任务在途）的批次由 Poller 推进，不插手；已取消的批次不拉回；
//   - 必须已有生图提示词（编排阶段整批失败的没有，重新编排会牵动整批，只能重新提交）；
//   - 原预留单必须已结束且没扣费（已退回 / 已过期 / 本来就没冻结）。仍 HELD（结果未知、待对账）或已扣费的拒绝：重做会把任务上的计费请求号换成新号，原单就脱离了任务的对账视野。只认计费服务的真实状态，镜像只作快速拒绝；
//   - 只读余额预检（Enough）不够直接 3003，不占轮次；之后 Hold 仍失败时 retryTask 退还轮次（确认没建单才退）。
//
// 计费：新号 studio-<task_id>-r<attempt>（轮次 CAS 独占），与提交同一入口码 / 档位 / 分辨率，冻结 1 张；
// 原单不再碰（不会二次退款）。之后照常：出图结算新单，失败退新单。
func (f *flow) userRetry(ctx context.Context, t *model.Task, b *model.Batch) error {
	if t.AdminLocked != 0 {
		return xerr.New(xerr.Conflict, "该任务已停止重试，请重新提交")
	}
	if t.Status != model.TaskFailed {
		return xerr.New(xerr.Conflict, "只有失败的任务可以重试")
	}
	switch b.Status {
	case model.BatchFailed, model.BatchPartial, model.BatchGenerating:
	case model.BatchCanceled:
		return xerr.New(xerr.Conflict, "批次已取消，不能重试")
	default:
		return xerr.New(xerr.Conflict, errBatchRunning)
	}
	if !t.PromptText.Valid || strings.TrimSpace(t.PromptText.String) == "" {
		return xerr.New(xerr.Conflict, "这张图还没生成提示词，无法单独重试，请重新提交")
	}
	if t.Attempt >= userRetryMaxAttempts {
		return xerr.New(xerr.Conflict, "这张图已重试多次，请调整素材后重新提交")
	}
	if !f.gwOn {
		return xerr.New(xerr.NotReady, "生图服务暂未开放，请稍后再试")
	}
	if t.BillingStatus == model.BillingHeld || t.BillingStatus == model.BillingSettled {
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
				logx.WithContext(ctx).Errorf("studio: 用户重试子任务 %d 查原预留单 %s 失败: %v", t.ID, old, err)
				return xerr.New(xerr.ServerError, "暂时无法核对计费，请稍后重试")
			}
			switch st {
			case "", resvRefunded, resvExpired: // 没冻结成功 / 已退回 / 已过期释放：原单已了结
			default:
				logx.WithContext(ctx).Errorf("studio: 用户重试子任务 %d 拒绝：原预留单 %s 状态 %s", t.ID, old, st)
				return xerr.New(xerr.Conflict, "上一次的点数还在结算中，请稍后再试")
			}
		}
	}
	if err := f.bill.CheckAccess(hctx, b.UserID, entry); err != nil { // 与提交同口径：能力开通校验
		return accessError(ctx, b.UserID, entry, err)
	}
	// 只读余额预检（报价 vs 余额，不冻结）：明显不够的直接回 3003，不占轮次。
	// 预检通过后 Hold 仍可能失败（并发扣点），retryTask 会在确认没建单后把轮次退还。
	charge, err := f.retryCharge(t, b)
	if err != nil {
		return err
	}
	if err := f.bill.Enough(hctx, charge); err != nil {
		return err
	}
	return f.retryTask(ctx, t, b, stepUserRetry)
}

// errBatchRunning 批次的原始流程（编排 / 首轮生图）还在进行中时的拒绝文案。
const errBatchRunning = "批次还在生成中，请等全部结束后再重试"

// onlyRetriesInFlight 批次处于生图中时，只有「在途的都是重做轮次」才允许再重试同批的其它失败任务：
// 同批已有一张在重试，批次被拉回生图中，此时其余失败任务也可以各自重试（不用一张张等）。
// 判据：除本任务外，每个子任务要么已终态，要么是在途的重做轮次（retriedInFlight：计费请求号带 -r<n>，用户重试 / 运营重做都算）；
// 只要还有首轮的在途任务（原始流程还在跑：排队等派发 / 生成中 / 交付中），一律拒绝，由 Poller 推进完再说。
// 并发重试各自 CAS 自己的任务，计数用原子增减；批次终态只在全部子任务到终态后由 converge 按实际计数重算。
func (f *flow) onlyRetriesInFlight(ctx context.Context, t *model.Task, b *model.Batch) error {
	tasks, err := f.repo.Task.ListByBatch(ctx, b.ID)
	if err != nil {
		return err
	}
	for _, o := range tasks {
		if o.ID == t.ID || model.TaskFinal(o.Status) || retriedInFlight(o) {
			continue
		}
		return xerr.New(xerr.Conflict, errBatchRunning)
	}
	return nil
}
