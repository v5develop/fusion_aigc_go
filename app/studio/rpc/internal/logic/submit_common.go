package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/workspace"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// BillingPrefix 子任务计费请求号前缀：billing.Charge.RequestID = "studio-<task_id>"（也写进 afv_studio_task.billing_request_id）。
const BillingPrefix = "studio-"

// taskIDStage 子任务号的步骤段：aigc.NewLifeTaskID(前缀, taskcenter.Ref(前缀, id), "gen")。
const taskIDStage = "gen"

// biller 提交 / 取消链路用到的计费调用；*billing.Service 满足。抽成接口便于单测替换。
type biller interface {
	Mode() string
	CheckAccess(ctx context.Context, userID int64, entryCode string) error
	Hold(ctx context.Context, c billing.Charge) (*billing.Reservation, error)
	Fail(ctx context.Context, userID int64, requestID, reason string)
}

// deps 提交 / 查询 / 取消共用的依赖。生产由 newDeps 从 svcCtx 组装，单测直接构造。
type deps struct {
	repo       *repo.Repo
	bill       biller
	mod        moderation.Checker
	owns       func(url string) bool                                  // 输入图是否本存储签发 / 素材库公有桶
	personalWS func(ctx context.Context, userID int64) (int64, error) // 个人空间 id，没有返回 0
	ready      func(k model.Kind) error                               // 依赖是否就绪（gateway / 媒体服务）
	prefix     func(k model.Kind) string                              // 子任务号前缀 tyo / pse / simg
	entry      func(k model.Kind) string                              // 入口码
	sync       func(ctx context.Context)                              // 任务中心同步一轮
	newTaskID  func(prefix string, id int64) string                   // 子任务号
	now        func() time.Time
}

// ownsSigned 输入图是用户自己成品的签名水印地址（本实例刚签出的 mrw:// 视图）：反查 rid 并确认该用户有这条成品。
// 免费档用户拿到的结果地址就是这种私有桶签名地址，不放行就没法把结果当作下一次生成的输入。
func (d *deps) ownsSigned(ctx context.Context, userID int64, u string) bool {
	rid := ridOfSignedURL(u, d.now())
	if rid == "" || d.repo == nil || d.repo.Task == nil {
		return false
	}
	ok, err := d.repo.Task.OwnsResultRID(ctx, userID, rid)
	if err != nil {
		logx.WithContext(ctx).Errorw("studio: own signed url check failed (check=studio_input_owns_signed)", logx.Field("err", err.Error()))
		return false
	}
	return ok
}

func newDeps(s *svc.ServiceContext) *deps {
	c := s.Config
	return &deps{
		repo: s.Repo, bill: s.Billing, mod: s.Moderation,
		owns: func(u string) bool {
			// 先过结构校验（无 userinfo / .. / 反斜杠），再认本存储签发或白名单前缀（scheme + host 全等、路径按段前缀）
			if !cleanHTTPURL(u) {
				return false
			}
			return storage.Owned(s.Storage, u) == nil || matchPrefixes(c.InputURLPrefixes, u)
		},
		personalWS: func(ctx context.Context, userID int64) (int64, error) {
			return workspace.PersonalID(ctx, s.Conn, userID)
		},
		ready: func(k model.Kind) error {
			if s.Gateway == nil || !s.Gateway.Enabled() {
				return xerr.New(xerr.NotReady, "生图服务暂未开放，请稍后再试")
			}
			if k == model.KindTryon && s.MediaSvc == nil {
				return xerr.New(xerr.NotReady, "商拍换装暂未开放，请稍后再试")
			}
			return nil
		},
		prefix: func(k model.Kind) string { return svc.Prefix(c, k) },
		entry:  func(k model.Kind) string { return svc.EntryCode(c, k) },
		sync: func(ctx context.Context) {
			if s.TaskCenter != nil {
				_ = s.TaskCenter.Sync(ctx) // 失败已在 taskcenter 里记日志
			}
		},
		newTaskID: func(prefix string, id int64) string {
			return aigc.NewLifeTaskID(prefix, taskcenter.Ref(prefix, id), taskIDStage)
		},
		now: time.Now,
	}
}

// slot 一个子任务在批次里的坐标。
type slot struct{ Model, Shot, Pose int32 }

// tryonSlots 商拍：模特 × 每位张数（model_index, shot_index），先模特后张数。
func tryonSlots(models, perModel int) []slot {
	out := make([]slot, 0, models*perModel)
	for m := 0; m < models; m++ {
		for s := 0; s < perModel; s++ {
			out = append(out, slot{Model: int32(m), Shot: int32(s)})
		}
	}
	return out
}

// poseSlots 姿态：每个姿势一个（pose_index）。
func poseSlots(poses int) []slot {
	out := make([]slot, 0, poses)
	for p := 0; p < poses; p++ {
		out = append(out, slot{Pose: int32(p)})
	}
	return out
}

// imageGenSlots 图片生成：count 张（shot_index）。
func imageGenSlots(count int) []slot {
	out := make([]slot, 0, count)
	for s := 0; s < count; s++ {
		out = append(out, slot{Shot: int32(s)})
	}
	return out
}

// urlCheck 一张要校验归属的输入图；field 是报错用的字段名（与 validate.go 的口径一致，如「模特图第 1 张」）。
type urlCheck struct{ field, url string }

// urlChecks 一组图按「<what>第 n 张」命名。
func urlChecks(what string, urls []string) []urlCheck {
	out := make([]urlCheck, 0, len(urls))
	for i, u := range urls {
		out = append(out, urlCheck{fmt.Sprintf("%s第 %d 张", what, i+1), u})
	}
	return out
}

// textCheck 一段要审核的文本。
type textCheck struct{ scene, text string }

// submitSpec 三种提交归一后的公共形态。
type submitSpec struct {
	Kind        model.Kind
	UserID      int64
	WorkspaceID int64
	RequestID   string
	Payload     map[string]any // 前端全量字段快照（归一后），encoding/json 按键排序，指纹稳定
	URLs        []urlCheck     // 要校验归属的输入图
	Texts       []textCheck    // 要审核的文本
	ModelGroup  string
	Resolution  string // 1k / 2k / 4k（已按 model group 归一，与实际下发、计费一致）
	Slots       []slot
	Warnings    []string // 归一告警（如 Seedream 4K → 2K），随 SubmitResp 回给前端
}

// payloadOf 快照 JSON 与指纹（SHA-256 十六进制）。
func payloadOf(p map[string]any) (json.RawMessage, string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

// errIdemConflict 同一幂等键、不同请求内容。语义对应 Java IDEMPOTENCY_CONFLICT(46009)；
// pkg/xerr 没有登记该码（码表须与 Java ErrorCode 一一对应），按接口文档用 RESOURCE_CONFLICT(4002)。
// 幂等只按 (user_id, request_id)：不做「同 payload 指纹 + 时间窗口」归并（换 key 的重新生成必须得到新批次）。
func errIdemConflict() error {
	return xerr.New(xerr.Conflict, "幂等键已被不同请求使用")
}

// submit 提交主流程：幂等 → 归属 → 审核 → 就绪 → 空间 → 准入 → 落库（批次提交中 + 子任务 + 回填任务号）→ 逐张冻结
// → 批次 提交中→排队 → 日志 → 任务中心同步。
func (d *deps) submit(ctx context.Context, sp *submitSpec) (*studio.SubmitResp, error) {
	start := d.now()
	log := logx.WithContext(ctx)
	payload, hash, err := payloadOf(sp.Payload)
	if err != nil {
		return nil, err
	}

	// 1. 幂等：同 (user_id, request_id) 已提交过
	if b, err := d.repo.Batch.GetByUserRequest(ctx, sp.UserID, sp.RequestID); err != nil {
		return nil, err
	} else if b != nil {
		return d.replay(ctx, b, hash)
	}

	// 2. 输入图归属
	for _, u := range sp.URLs {
		if !d.owns(u.url) && !d.ownsSigned(ctx, sp.UserID, u.url) {
			return nil, xerr.Param(u.field + " 不是本服务上传的地址")
		}
	}
	// 3. 文本审核
	for _, t := range sp.Texts {
		if err := checkText(ctx, d.mod, t.scene, t.text); err != nil {
			return nil, err
		}
	}
	// 4. 依赖就绪
	if err := d.ready(sp.Kind); err != nil {
		return nil, err
	}
	// 5. 工作空间：0 回落个人空间（查不到仍为 0，素材入库时 asset 会兜底建空间）
	ws := sp.WorkspaceID
	if ws == 0 && d.personalWS != nil {
		if id, err := d.personalWS(ctx, sp.UserID); err != nil {
			log.Errorf("studio: 查用户 %d 个人空间失败: %v", sp.UserID, err)
		} else {
			ws = id
		}
	}
	// 6. 准入（与计费开关无关）
	entry := d.entry(sp.Kind)
	if err := d.bill.CheckAccess(ctx, sp.UserID, entry); err != nil {
		return nil, accessError(ctx, sp.UserID, entry, err)
	}

	// 7. 落库：批次（提交中）+ 子任务 + 回填任务号（同一事务）。提交中的批次 Poller 不编排，冻结完才 CAS → 排队
	b := &model.Batch{
		UserID: sp.UserID, WorkspaceID: ws, Kind: sp.Kind, RequestID: sp.RequestID,
		Payload: payload, PayloadHash: hash, Status: model.BatchSubmitting, TaskTotal: int32(len(sp.Slots)),
	}
	var tasks []*model.Task
	err = d.repo.RunInTx(ctx, func(ctx context.Context, tx *repo.Repo) error {
		if err := tx.Batch.Insert(ctx, b); err != nil {
			return err
		}
		rows := make([]*model.Task, 0, len(sp.Slots))
		for _, s := range sp.Slots {
			rows = append(rows, &model.Task{
				BatchID: b.ID, UserID: sp.UserID, Kind: sp.Kind,
				ModelIndex: s.Model, ShotIndex: s.Shot, PoseIndex: s.Pose,
				ModelGroup: sp.ModelGroup, Status: model.TaskQueued,
			})
		}
		if err := tx.Task.BulkInsert(ctx, rows); err != nil {
			return err
		}
		// 主键以库里读回的为准（不依赖多行插入的自增连续性）
		var err error
		if tasks, err = tx.Task.ListByBatch(ctx, b.ID); err != nil {
			return err
		}
		if len(tasks) != len(sp.Slots) {
			return fmt.Errorf("studio: 批次 %d 子任务数 %d 与预期 %d 不符", b.ID, len(tasks), len(sp.Slots))
		}
		ids := make(map[int64]string, len(tasks))
		prefix := d.prefix(sp.Kind)
		for _, t := range tasks {
			t.TaskID.String, t.TaskID.Valid = d.newTaskID(prefix, t.ID), true
			t.BillingRequestID.String, t.BillingRequestID.Valid = BillingPrefix+t.TaskID.String, true
			ids[t.ID] = t.TaskID.String
		}
		return tx.Task.BackfillTaskIDs(ctx, ids, BillingPrefix)
	})
	if err != nil {
		if repo.IsDuplicate(err) {
			// 并发同键：对方先落库了，按幂等读回
			if b2, err2 := d.repo.Batch.GetByUserRequest(ctx, sp.UserID, sp.RequestID); err2 == nil && b2 != nil {
				return d.replay(ctx, b2, hash)
			}
		}
		return nil, err
	}

	// 8. 逐张冻结（与 RPC ctx 的取消脱钩，独立 60s 超时）：每张成功立刻写计费镜像；
	// 任一失败：这张也 Fail 一次（冻结可能已落库只是响应丢了），已冻结的逐个退回，批次与子任务置失败
	hctx, cancel := detached(ctx)
	defer cancel()
	var points int64
	st := billingStatusOf(d.bill.Mode())
	held := make([]*model.Task, 0, len(tasks))
	for _, t := range tasks {
		r, err := d.bill.Hold(hctx, d.charge(sp, t))
		if err != nil {
			d.holdFailed(ctx, b, held, t, err)
			return nil, err
		}
		if r != nil {
			points += r.PricePoints
		}
		if st != model.BillingNone {
			if err := d.repo.Task.SetBilling(hctx, t.ID, t.BillingRequestID.String, st); err != nil {
				log.Errorf("studio: 子任务 %d 写计费状态失败（退款不依赖它）: %v", t.ID, err)
			}
		}
		held = append(held, t)
	}
	if points > 0 {
		if err := d.repo.Batch.SetEstimatedPoints(hctx, b.ID, points); err != nil {
			log.Errorf("studio: 批次 %d 写预估点数失败: %v", b.ID, err)
		}
	}
	// 9. 提交中 → 排队；没抢到（库不可用 / 已被兜底置失败）按失败善后
	ok, err := d.repo.Batch.UpdateStatus(hctx, b.ID, model.BatchSubmitting, model.BatchQueued, nil)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("批次状态已变化")
		}
		log.Errorf("studio: 批次 %d 提交中→排队失败: %v", b.ID, err)
		d.holdFailed(ctx, b, held, nil, err)
		return nil, xerr.New(xerr.ServerError, "提交失败，点数已退回，请稍后重试")
	}
	d.stepLog(hctx, b.ID, model.StepLog{Step: "submitted", Ms: d.now().Sub(start).Milliseconds(), TaskTotal: len(tasks)})
	d.sync(ctx)

	return &studio.SubmitResp{BatchId: b.ID, TaskTotal: int32(len(tasks)), EstimatedPoints: points, TaskIds: taskIDs(tasks), Warnings: sp.Warnings}, nil
}

// replay 幂等命中：指纹一致返回原批次（replayed=true），不一致报幂等冲突。
func (d *deps) replay(ctx context.Context, b *model.Batch, hash string) (*studio.SubmitResp, error) {
	if b.PayloadHash != hash {
		return nil, errIdemConflict()
	}
	tasks, err := d.repo.Task.ListByBatch(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	return &studio.SubmitResp{
		BatchId: b.ID, TaskTotal: b.TaskTotal, EstimatedPoints: b.EstimatedPoints, TaskIds: taskIDs(tasks), Replayed: true,
	}, nil
}

// charge 一个子任务的计费请求：一张图一张预留单。模型组取子任务上落库的组（与实际出图一致）。
func (d *deps) charge(sp *submitSpec, t *model.Task) billing.Charge {
	c := quoteCharge(d.entry(sp.Kind), sp.UserID, orDefault(t.ModelGroup, sp.ModelGroup), sp.Resolution)
	c.RequestID = BillingPrefix + t.TaskID.String
	return c
}

// quoteCharge 一张图的计费请求（不带预留单号）：提交冻结、提交前报价（Quote）、重做冻结与余额预检共用，保证同价。
// 价格 = f(入口, 模型组, 分辨率)：modelGroup 按档位默认模型的 gateway_model_group 选分模型档位
// （该入口没配这个组的档位时退回默认档位），resolution 选该档位单价行的分辨率档。
func quoteCharge(entry string, userID int64, modelGroup, resolution string) billing.Charge {
	return billing.Charge{
		UserID: userID, EntryCode: entry,
		Kind: billing.KindQuick, Count: 1,
		ModelGroup: modelGroup,
		Tier:       tierOf(entry, resolution),
		Resolution: strings.ToUpper(resolution), // afv_model_pricing.resolution 用 1k/2k/4k（库排序规则不分大小写）；匹配不到退回不限分辨率的单价
	}
}

// tierOf 分辨率档位：只有 billing.Targets 给该入口登记了 Tiers 且包含该分辨率时才分档，否则不分档。
func tierOf(entry, resolution string) string {
	for _, t := range billing.Targets {
		if t.Entry != entry {
			continue
		}
		for _, tier := range t.Tiers {
			if strings.EqualFold(tier, resolution) {
				return tier
			}
		}
	}
	return ""
}

// billingStatusOf 冻结成功后子任务的计费状态镜像。
func billingStatusOf(mode string) int8 {
	switch mode {
	case billing.ModeOn:
		return model.BillingHeld
	case billing.ModeShadow:
		return model.BillingShadow
	}
	return model.BillingNone
}

// holdFailed 冻结中途失败的善后：失败那张（failed，可为 nil）也 Fail 一次，退回已冻结的、子任务全部置失败、批次 提交中→失败。
// 用与调用方取消脱钩的独立 60s 超时；都是尽力而为（只记日志），没做完的由 Poller 的提交中兜底（reapSubmitting）收尾。
func (d *deps) holdFailed(ctx context.Context, b *model.Batch, held []*model.Task, failed *model.Task, cause error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	log := logx.WithContext(ctx)
	const refundReason = "提交未完成，点数已退回"
	if failed != nil && d.bill.Mode() == billing.ModeOn {
		d.bill.Fail(ctx, b.UserID, failed.BillingRequestID.String, refundReason)
	}
	for _, t := range held {
		d.bill.Fail(ctx, b.UserID, t.BillingRequestID.String, refundReason)
		if err := d.repo.Task.SetBilling(ctx, t.ID, "", model.BillingRefunded); err != nil {
			log.Errorf("studio: 子任务 %d 写退款状态失败: %v", t.ID, err)
		}
	}
	reason := "点数冻结失败"
	if _, msg, ok := xerr.Parse(cause); ok {
		reason = msg
	}
	if _, err := d.repo.Task.FailQueued(ctx, b.ID, reason); err != nil {
		log.Errorf("studio: 批次 %d 子任务置失败出错: %v", b.ID, err)
	}
	if _, err := d.repo.Batch.UpdateStatus(ctx, b.ID, model.BatchSubmitting, model.BatchFailed,
		map[string]any{"error_reason": repo.Trunc(reason, 255), "task_failed": b.TaskTotal}); err != nil {
		log.Errorf("studio: 批次 %d 置失败出错: %v", b.ID, err)
	}
	d.stepLog(ctx, b.ID, model.StepLog{Step: "hold_failed", TaskTotal: int(b.TaskTotal), Detail: reason})
	d.sync(ctx)
}

func (d *deps) stepLog(ctx context.Context, batchID int64, e model.StepLog) {
	if e.At == 0 {
		e.At = d.now().UnixMilli()
	}
	if err := d.repo.Batch.AppendStepLog(ctx, batchID, e); err != nil {
		logx.WithContext(ctx).Errorf("studio: 批次 %d 写 step_log 失败: %v", batchID, err)
	}
}

func taskIDs(tasks []*model.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.TaskID.String)
	}
	return out
}

// accessError 准入失败翻译成用户口径；内部原因只记日志。
func accessError(ctx context.Context, userID int64, entry string, err error) error {
	if errors.Is(err, billing.ErrNoFeature) {
		return xerr.New(xerr.Forbidden, "暂未开通该功能")
	}
	if _, _, ok := xerr.Parse(err); ok {
		return err
	}
	logx.WithContext(ctx).Errorf("studio: 用户 %d 入口 %s 准入校验失败: %v", userID, entry, err)
	return xerr.New(xerr.ServerError, "暂时无法计费，请稍后重试")
}

// checkText 内容审核：命中敏感词返回参数错误；读词库失败只记日志不拦截（与其它模块一致）。
func checkText(ctx context.Context, m moderation.Checker, scene, text string) error {
	if m == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	r, err := m.Check(ctx, scene, text)
	if err != nil {
		logx.WithContext(ctx).Errorf("moderation %s: %v", scene, err)
		return nil
	}
	if !r.Passed {
		logx.WithContext(ctx).Infof("moderation blocked scene=%s label=%s", scene, r.Label)
		return xerr.New(xerr.ParamError, "内容包含敏感信息，请修改后重试")
	}
	return nil
}

// ---------------- 三种提交 → submitSpec ----------------

func tryonSpec(in *studio.SubmitTryonReq, v *TryonInput) *submitSpec {
	garments := make([]map[string]string, 0, len(v.Garments))
	urls := urlChecks("模特图", v.Models)
	for i, g := range v.Garments {
		garments = append(garments, map[string]string{"url": g.Url, "role": g.Role, "angle": g.Angle})
		urls = append(urls, urlCheck{fmt.Sprintf("服装图第 %d 张", i+1), g.Url})
	}
	texts := []textCheck{{"studio_tryon_prompt", in.Prompt}, {"studio_tryon_fabric", in.Fabric}}
	for _, m := range []map[string]string{v.Lengths, v.Sleeves} {
		for _, s := range m {
			texts = append(texts, textCheck{"studio_tryon_attr", s})
		}
	}
	// 自选细节框的部位名会进 sidecar 图片标签与生图提示词（局部细节（label）），同属用户文本，一并审核
	for _, c := range v.DetailCrops {
		texts = append(texts, textCheck{"studio_tryon_attr", c.Label})
	}
	sp := &submitSpec{
		Kind: model.KindTryon, UserID: in.UserId, WorkspaceID: in.WorkspaceId, RequestID: strings.TrimSpace(in.RequestId),
		Payload: map[string]any{
			"workspace_id": in.WorkspaceId, "model_image_urls": v.Models, "garment_images": garments,
			"items": v.Items, "set_mode": v.SetMode, "lengths": v.Lengths, "sleeves": v.Sleeves,
			"fabric": in.Fabric, "prompt": in.Prompt, "model": v.ModelName, "resolution": v.Resolution,
			"aspect_ratio": v.AspectRatio, "count_per_model": v.CountPerModel,
		},
		URLs: urls, Texts: texts, ModelGroup: v.ModelGroup, Resolution: v.Resolution, Warnings: v.Warnings,
		Slots: tryonSlots(len(v.Models), int(v.CountPerModel)),
	}
	withColorFix(sp, in.ColorFix)
	// 精细读图 / 用户自选细节框：只在用到时写入，不用的提交 payload（及其幂等哈希）与旧版逐字一致
	if v.FineRead {
		sp.Payload["fine_read"] = true
	}
	if v.DetailCropsManual {
		crops := v.DetailCrops
		if crops == nil {
			crops = []pipeline.UserCrop{}
		}
		sp.Payload["detail_crops_manual"] = true
		sp.Payload["detail_crops"] = crops
	}
	return sp
}

// withColorFix 「默认校色」开关写进 payload（GET 批次详情的 batch.payload 原样回显，前端据此给结果套默认校色参数）。
// 只在开启时写入：不开的提交 payload（及其幂等哈希）与旧版逐字一致，老批次重放不受影响。生图与计费都不看它。
func withColorFix(sp *submitSpec, on bool) {
	if on {
		sp.Payload["color_fix"] = true
	}
}

func poseSpec(in *studio.SubmitPoseReq, v *PoseInput) *submitSpec {
	prompts := make(map[string]string, len(v.PosePrompts))
	texts := []textCheck{{"studio_pose_prompt", in.GlobalPrompt}}
	for i, p := range v.PosePrompts {
		prompts[fmt.Sprint(i)] = p
		texts = append(texts, textCheck{"studio_pose_prompt", p})
	}
	sp := &submitSpec{
		Kind: model.KindPose, UserID: in.UserId, WorkspaceID: in.WorkspaceId, RequestID: strings.TrimSpace(in.RequestId),
		Payload: map[string]any{
			"workspace_id": in.WorkspaceId, "source_image_url": v.Source, "pose_image_urls": v.Poses,
			"global_prompt": in.GlobalPrompt, "pose_prompts": prompts, "model": v.ModelName,
			"resolution": v.Resolution, "aspect_ratio": v.AspectRatio,
		},
		URLs: append([]urlCheck{{"人物图", v.Source}}, urlChecks("姿势图", v.Poses)...), Texts: texts, ModelGroup: v.ModelGroup, Resolution: v.Resolution, Warnings: v.Warnings,
		Slots: poseSlots(len(v.Poses)),
	}
	// 多人配对：编排时传给 sidecar pose/prompt，历史回填时前端按它恢复配对。
	// 只在传了配对时写入，不带配对的提交 payload（及其幂等哈希）与旧版逐字一致
	if len(v.Mappings) > 0 {
		sp.Payload["source_person_count"] = v.SourcePersonCount
		sp.Payload["mappings"] = v.Mappings
	}
	withColorFix(sp, in.ColorFix)
	return sp
}

func imageGenSpec(in *studio.SubmitImageGenReq, v *ImageGenInput) *submitSpec {
	return &submitSpec{
		Kind: model.KindImageGen, UserID: in.UserId, WorkspaceID: in.WorkspaceId, RequestID: strings.TrimSpace(in.RequestId),
		Payload: map[string]any{
			"workspace_id": in.WorkspaceId, "prompt": v.Prompt, "image_urls": v.Images, "model": v.ModelName,
			"resolution": v.Resolution, "aspect_ratio": v.AspectRatio, "count": v.Count, "rewrite_prompt": in.RewritePrompt,
		},
		URLs: urlChecks("参考图", v.Images), Texts: []textCheck{{"studio_imagegen_prompt", v.Prompt}}, ModelGroup: v.ModelGroup, Resolution: v.Resolution, Warnings: v.Warnings,
		Slots: imageGenSlots(int(v.Count)),
	}
}
