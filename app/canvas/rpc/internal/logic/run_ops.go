package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/lease"
	"aigc-local-life/pkg/taskevent"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// RunNode 执行一个节点：幂等 → 校验（连线规则 / 引用 / 参数）→ 落库 → 冻结（gateway 后端）→ 内联提交后端。
func (c *core) RunNode(ctx context.Context, in *canvas.RunNodeReq) (*canvas.RunNodeResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	bearer := c.rememberBearer(ctx, in.UserId)
	runID := strings.TrimSpace(in.RunId)
	switch {
	case runID == "":
		return nil, xerr.New(xerr.ParamMissing, "缺少 Idempotency-Key（run_id）")
	case len(runID) > 64:
		return nil, xerr.Param("run_id 最长 64 字符")
	case strings.TrimSpace(in.NodeId) == "" || len(in.NodeId) > 64:
		return nil, xerr.Param("node_id 不合法")
	case in.Request == nil:
		return nil, xerr.New(xerr.ParamMissing, "缺少执行参数")
	}
	// 蒙版 dataURL 先校验尺寸并拉入媒体服务，request 列只存 mask_rid
	if err := c.normalizeMask(ctx, in.Request); err != nil {
		return nil, err
	}
	// 图片分辨率按模型组归一（Seedream 4K → 2K）：快照、冻结 / 结算档位、gateway 请求都用归一值
	warns := c.normalizeRunSize(in.Request)
	reqJSON, _ := json.Marshal(toStored(in.Request))

	// 1. 幂等
	if old, err := c.s.Repo.Run.GetByUserRun(ctx, in.UserId, runID); err != nil {
		return nil, err
	} else if old != nil {
		return c.replay(old, in, reqJSON, warns)
	}
	// 2. 画布与连线规则
	cv, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.CanvasId)
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, xerr.NotFoundf("画布")
	}
	doc, err := ParseSnapshot(cv.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	lookup := c.refResolver(ctx, cv.ID)
	sp, err := c.buildSpec(in.Request, lookup)
	if err != nil {
		return nil, err
	}
	// 上游引用受理时解析一次、冻结进落库请求：此后每次（重新）派发都用这批产物，不随上游重跑而变
	// （同一幂等键下请求内容不变，网关不会判幂等冲突；也不会拿用户没提交过的输入出图）
	st := toStored(in.Request)
	st.Frozen = lookup.frozenOut()
	reqJSON, _ = json.Marshal(st)
	if err := c.checkRefs(doc, in, sp); err != nil {
		return nil, err
	}
	if err := c.ready(ctx, sp, bearer); err != nil {
		return nil, err
	}
	// 3. 同一节点同一时刻只允许一个 run（这里先查一次给友好提示；并发靠 uk_active_node 唯一键兜住）
	if act, err := c.s.Repo.Run.ActiveOnNode(ctx, cv.ID, in.NodeId); err != nil {
		return nil, err
	} else if act != nil {
		return nil, errActiveOnNode
	}
	// 智能体给了点数上限（max_points）：冻结前按与冻结同源的报价比较，超出不落库、不冻结（agent_budget.go）
	if err := c.checkBudget(ctx, in.UserId, sp); err != nil {
		return nil, err
	}
	// 4. 准入（gateway 后端由 Go 计费）
	if sp.Backend == model.BackendGateway {
		if err := c.s.Billing.CheckAccess(ctx, in.UserId, sp.EntryCode); err != nil {
			// 没开通 → Forbidden；入口没配价格（ErrNoOffer）/ 库异常 → ServerError，日志带判据（见 billing.UserError）
			return nil, billing.UserError(ctx, in.UserId, sp.EntryCode, err)
		}
	}
	// 5. 落库（提交中）：插入与回填 task_id / billing_request_id 在同一事务，行可见时两者必然已有值
	run := &model.NodeRun{
		CanvasID: cv.ID, NodeID: in.NodeId, UserID: in.UserId, RunID: runID, Kind: sp.Kind, Request: reqJSON,
		ModelGroup: sp.ModelGroup, Backend: sp.Backend, Status: model.RunSubmitting, RequestedCount: int32(sp.Count),
		BillingStatus: initialBilling(sp.Backend), CostCNY: "0",
	}
	if err := c.insertRun(ctx, run, sp); err != nil {
		switch {
		case repo.IsActiveConflict(err):
			return nil, errActiveOnNode
		case repo.IsDuplicate(err):
			if old, err2 := c.s.Repo.Run.GetByUserRun(ctx, in.UserId, runID); err2 == nil && old != nil {
				return c.replay(old, in, reqJSON, warns)
			}
		}
		return nil, err
	}
	run.CreatedAt = c.now()
	c.evMark(ctx, run, "go.canvas/created", "受理并落库", 0, runAttrs(run, taskevent.Attrs{"entry_code": sp.EntryCode, "count": sp.Count, "points": run.Points}))
	c.evStart(ctx, run, stQueue, 0, runAttrs(run, nil))
	// 6. 冻结（每个 run 一张预留单）→ 一次 CAS 写 billing_status + points 并转排队
	if err := c.holdAndQueue(ctx, run, sp); err != nil {
		return nil, err
	}
	// 7. 内联提交（暂时性错误留在排队，由轮询器重提）。先抢 run 的租约：轮询器（本实例或别的实例）
	//    在转排队后的这一瞬间已经抢到，就交给它提交，这里直接回「排队」，避免同一 run 被提交两次。
	if err := c.dispatchInline(ctx, run, sp, bearer); err != nil {
		return nil, err
	}
	// 任务中心同步移出请求路径（全量窗口同步 1–2s）：异步合并执行，失败重试，Poller 每轮也会兜底同步
	c.s.TaskSync.Kick()
	return &canvas.RunNodeResp{
		RequestId: strconv.FormatInt(run.ID, 10), RunId: runID, Status: model.StatusText(run.Status), Credits: run.Points,
		Warnings: mergeWarnings(warns, sp.Warnings), SettleMode: sp.SettleMode, MaxPoints: maxPointsOf(sp.SettleMode, run.Points),
	}, nil
}

// dispatchInline 持有租约时内联提交；抢不到（或抢租约出错）就留在排队交给轮询器。
// 提交成功转运行中的 gateway run：放掉租约后立刻交给长轮询协程（不等下一轮扫描）。
func (c *core) dispatchInline(ctx context.Context, run *model.NodeRun, sp *runSpec, bearer string) error {
	owner := lease.Owner()
	ok, err := c.s.Repo.Run.AcquireLease(ctx, run.ID, owner, runLeaseTTL)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 内联提交抢租约失败，交给轮询器: %v", run.ID, err)
		return nil
	}
	if !ok {
		return nil
	}
	err = c.dispatch(ctx, run, sp, bearer)
	rctx, cancel := detached(ctx)
	if rerr := c.s.Repo.Run.ReleaseLease(rctx, run.ID, owner); rerr != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 释放租约失败（到期自动失效）: %v", run.ID, rerr)
	}
	cancel()
	c.handOff(run)
	return err
}

// mergeWarnings 合并告警（去重、保序）。
func mergeWarnings(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, w := range l {
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	return out
}

func initialBilling(b model.Backend) string {
	switch b {
	case model.BackendJava:
		return model.BillJava
	case model.BackendStudio:
		return model.BillStudio
	}
	return model.BillNone
}

// charge 一个 run 的计费请求。
func (c *core) charge(userID int64, requestID string, sp *runSpec) billing.Charge {
	ch := billing.Charge{UserID: userID, RequestID: requestID, EntryCode: sp.EntryCode, Kind: billing.KindQuick, Count: max(1, sp.Count)}
	switch sp.Kind {
	case model.KindImage:
		ch.Resolution = strings.ToUpper(sp.Size)
		// 按实际出图模型选档（Java V1.0.215 给 canvas_image 建了按模型组分价的档位）：GPT Image 2.6 / 2.5、Banana、
		// Seedream 各扣各的价；该组没有档位时 pickVariant 退回 canvas_image_standard（旧配置照旧）。
		ch.ModelGroup = sp.ModelGroup
		// 按请求参数冻结（Java V1.0.223，只对 ACTUAL_COST 且配了 freeze_cap_json 的载体生效，如 GPT Image 2.6）：
		// 质量与派发同一取值，参考图张数 = 派发送网关的张数（每张出图一次调用、各带全部参考图）
		// PromptChars / 参考图像素给 token 模型估算冻结（GPT Image 2.5 标准版 / 2.6）；参考图像素受理时没有，0 = 未知，
		// 按网关缩小后的上限估（RefPixels 留空、只给张数）
		ch.Usage = &billing.ImageUsage{Quality: imageQuality(sp), RefImages: len(sp.Images), PromptChars: utf8.RuneCountInString(sp.Prompt)}
		for _, im := range sp.Images {
			// 已知宽高按实际像素（网关缩小规则在 pkg/billing 里按 2048² 封顶），未知为 0 → 按上限
			ch.Usage.RefPixels = append(ch.Usage.RefPixels, int64(im.W)*int64(im.H))
		}
	case model.KindVideo:
		ch.Kind, ch.Count, ch.Seconds, ch.Resolution = billing.KindVideo, 1, sp.DurationS, strings.ToLower(sp.Size)
	}
	return ch
}

// replay 幂等命中：同 body 返回原 run，不同 body 4002。
func (c *core) replay(old *model.NodeRun, in *canvas.RunNodeReq, reqJSON []byte, warns []string) (*canvas.RunNodeResp, error) {
	if old.CanvasID != in.CanvasId || old.NodeID != in.NodeId || !sameRequest(old.Request, reqJSON) {
		return nil, xerr.New(xerr.Conflict, "同一个 run_id 已用于不同的执行参数，请换一个 run_id")
	}
	return &canvas.RunNodeResp{
		RequestId: strconv.FormatInt(old.ID, 10), RunId: old.RunID, Status: model.StatusText(old.Status),
		Replayed: true, Credits: old.Points, Warnings: warns,
	}, nil
}

// checkRefs 执行时按保存的快照再校一遍连线规则。
func (c *core) checkRefs(doc *Doc, in *canvas.RunNodeReq, sp *runSpec) error {
	rc := RunCheck{NodeID: in.NodeId, Kind: string(sp.Kind)}
	for _, r := range in.Request.References {
		u := r.Image
		if NormalizeHandle(r.Handle) == HandleVideo {
			u = r.Video
		}
		rc.Refs = append(rc.Refs, RefInput{Handle: r.Handle, NodeID: r.NodeId, URL: u})
	}
	switch sp.Kind {
	case model.KindImage, model.KindVideo:
		rc.ImageLimit = c.catalogImageRefs(sp.Model)
		if n := doc.NodeByID()[in.NodeId]; n != nil {
			if v := c.imageLimit(n); v > 0 && (rc.ImageLimit <= 0 || v < rc.ImageLimit) {
				rc.ImageLimit = v
			}
		}
		rc.VideoLimit = max(1, sp.Model.MaxReferenceVideos)
	default:
		rc.ImageLimit = 1
	}
	if rc.ImageLimit <= 0 {
		rc.ImageLimit = defaultRefs
	}
	return ValidateRun(doc, rc)
}

// ready 后端是否就绪。
func (c *core) ready(ctx context.Context, sp *runSpec, bearer string) error {
	switch sp.Backend {
	case model.BackendGateway:
		if !c.s.Gateway.Enabled() {
			return xerr.New(xerr.NotReady, "生成服务暂未开放，请稍后再试")
		}
		if !c.modelReady(sp.ModelGroup) {
			return xerr.New(xerr.NotReady, "该模型暂不可用，请换一个模型")
		}
	case model.BackendMedia:
		if c.s.Media == nil {
			return xerr.New(xerr.NotReady, "裁剪暂未开放，请稍后再试")
		}
	case model.BackendJava:
		if bearer == "" && viaMcpToken(ctx) {
			return javaToolUnsupported()
		}
		if !c.s.Java.Enabled() {
			return xerr.New(xerr.NotReady, "该工具暂未开放，请稍后再试")
		}
		if bearer == "" {
			if viaSvc(ctx) {
				return loginRequired()
			}
			return xerr.New(xerr.NotReady, "该工具需要登录令牌（Authorization），请重新登录后再试")
		}
	case model.BackendStudio:
		if c.s.Studio == nil {
			return xerr.New(xerr.NotReady, "模特试穿暂未开放，请稍后再试")
		}
	}
	return nil
}

// ownedRun 取用户自己的 run。
func (c *core) ownedRun(ctx context.Context, userID, id int64) (*model.NodeRun, error) {
	if err := needUser(userID); err != nil {
		return nil, err
	}
	r, err := c.s.Repo.Run.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.UserID != userID || r.Deleted == 1 {
		return nil, xerr.NotFoundf("执行记录")
	}
	return r, nil
}

func (c *core) GetRun(ctx context.Context, in *canvas.RunIdReq) (*canvas.RunResult, error) {
	c.rememberBearer(ctx, in.UserId)
	r, err := c.ownedRun(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	return c.toResult(ctx, r), nil
}

// CancelRun 取消执行。产品决策：退款只有一个主人，且只在确认没发给供应商之后退。
//   - 排队 / 提交中（还没转运行中）：只打「已请求取消」标记（cancel_request=1），不改状态、不退款，由持有 run 的一方收尾——
//     派发器提交前抢 run 的 CAS 带「未请求取消」条件，抢不到就确认网关无已发出调用后结束并退款；已开始提交的，提交后转运行中的
//     CAS 同样带条件，没抢到就取消本次拿到的调用：全部取消成功才退款，有被拒（已发给供应商）的转运行中按实际结算（见 requestCancel）。
//   - 运行中 gateway：先取消上游调用，全部取消成功才退回冻结；已发给供应商的不能取消（4002 + reason CANCEL_VENDOR_SENT，
//     「已开始生成，无法取消，完成后按实际用量结算」），run 照常完成、按实际用量结算；studio 取消批次（由 studio 退款）。
//   - 已提交的 Java 工具用本次请求的 Bearer 调 Java 取消接口真实取消（由 Java 退款，Go 不退点数）；取消时 Java 已成功则按成功交付；
//     没有有效登录态时退化为停止跟踪（error_reason 注明可能仍会执行并扣费）；Java 暂时不可达时 run 保持运行中、后台重试取消（见 java_cancel.go）。
//   - 已取消的再取消：幂等返回当前结果；已成功 / 失败的 4002。
func (c *core) CancelRun(ctx context.Context, in *canvas.RunIdReq) (*canvas.RunResult, error) {
	r, err := c.ownedRun(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	for range 2 { // 打标记前一刻状态变了（转运行中 / 已结束）时按新状态再走一遍
		switch {
		case r.Status == model.RunCanceled:
			return c.toResult(ctx, r), nil
		case model.RunFinal(r.Status):
			return nil, xerr.New(xerr.Conflict, "执行已结束，不能取消")
		case javaLive(r): // 运行中的 Java 任务，或排队中已发过请求、可按幂等键找回的
			if viaMcpToken(ctx) { // 取消要用用户本人的 Java 登录令牌：MCP 令牌不借用缓存（agent_auth.go）
				return nil, javaToolUnsupported()
			}
			ended, pending := c.stopJava(ctx, r, stopUser, msgUserCanceled, c.rememberBearer(ctx, in.UserId), 0)
			if !ended && !pending {
				return nil, xerr.New(xerr.Conflict, "执行状态已变化，请刷新")
			}
			if got, err := c.s.Repo.Run.Get(ctx, r.ID); err == nil && got != nil {
				r = got // 可能已按成功交付（候选）或仍在运行中（待重试取消）
			}
			return c.toResult(ctx, r), nil
		case r.Status == model.RunQueued || r.Status == model.RunSubmitting:
			got, done, err := c.requestCancel(ctx, r, in.UserId)
			if done || err != nil {
				return got, err
			}
			if r, err = c.ownedRun(ctx, in.UserId, in.Id); err != nil {
				return nil, err
			}
			continue
		}
		return c.cancelRunning(ctx, r)
	}
	return nil, xerr.New(xerr.Conflict, "执行状态已变化，请刷新")
}

// cancelRunning 运行中的 run（非 Java）用户取消。
func (c *core) cancelRunning(ctx context.Context, r *model.NodeRun) (*canvas.RunResult, error) {
	if ids := callIDs(r); r.Backend == model.BackendGateway && len(ids) > 0 && c.s.Gateway.Enabled() {
		// 已有 gateway 调用：先取消上游，全部取消成功才结束并退款；任一已发给供应商 → 不结束、不退款，
		// 任务照常完成、按实际用量结算（竞态处理见 userCancelGatewayCalls）
		if err := c.userCancelGatewayCalls(ctx, r, ids); err != nil {
			return nil, err
		}
		if !c.endRun(ctx, r, r.Status, model.RunCanceled, "CANCELED", msgUserCanceled, true, false) {
			// 调用都已取消：轮询器可能先一步按「全部已取消」收尾（同样退款），以当前状态为准
			if got, err := c.s.Repo.Run.Get(ctx, r.ID); err == nil && got != nil && model.RunFinal(got.Status) {
				return c.toResult(ctx, got), nil
			}
			return nil, xerr.New(xerr.Conflict, "执行状态已变化，请刷新")
		}
		c.s.TaskSync.Kick()
		return c.toResult(ctx, r), nil
	}
	// 运行中、没有网关调用（studio 批次 / 网关未配置）：结束后取消上游（studio 由 studio 退款）
	if !c.endRun(ctx, r, r.Status, model.RunCanceled, "CANCELED", msgUserCanceled, true, false) {
		return nil, xerr.New(xerr.Conflict, "执行状态已变化，请刷新")
	}
	c.cancelUpstream(ctx, r, "用户取消")
	c.s.TaskSync.Kick()
	return c.toResult(ctx, r), nil
}

// defaultCancelWait 排队 / 提交中取消后，CancelRun 等派发器收尾的上限（提交一般 1–2 秒内返回）。
const defaultCancelWait = 3 * time.Second

// cancelPollInterval 等待期间重读 run 的间隔。
const cancelPollInterval = 200 * time.Millisecond

// requestCancel 排队 / 提交中的用户取消：只打「已请求取消」标记（不改状态、不退款），然后
//  1. 排队中（此刻没有派发器在提交时）就地收尾：抢得到行租约 → 本次请求即为持有方，确认网关无已发出调用后结束并退款
//     （closeQueuedCancel）；抢不到说明派发器 / 排队推进正拿着它，它抢 run 的 CAS 会看到标记并收尾。
//  2. 短暂等待结果（cancelWait）：到终态 → 回结果；转运行中且取消被拒 → 4002 + reason CANCEL_VENDOR_SENT（与运行中取消同一提示）；
//     仍未定 → 回当前状态 + cancel_state=requested（前端「取消中」，继续轮询，很快到 cancelled 或回到 running）。
//
// done=false 表示打标记前一刻 run 已离开排队 / 提交中（转运行中 / 已结束），调用方按新状态重走取消。重复取消幂等（标记已在就直接等结果）。
func (c *core) requestCancel(ctx context.Context, r *model.NodeRun, userID int64) (*canvas.RunResult, bool, error) {
	log := logx.WithContext(ctx)
	ok, err := c.s.Repo.Run.RequestCancel(ctx, r.ID)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		cur, err := c.s.Repo.Run.Get(ctx, r.ID)
		if err != nil {
			return nil, true, err
		}
		if cur == nil || (cur.Status != model.RunQueued && cur.Status != model.RunSubmitting) || cur.CancelRequest != model.CancelRequested {
			return nil, false, nil
		}
		r = cur // 已请求过：幂等，接着收尾 / 等结果
	} else {
		r.CancelRequest = model.CancelRequested
		log.Infof("canvas: run %d 用户在%s阶段取消，已登记取消请求，确认没发给供应商后再结束并退款", r.ID, model.StatusText(r.Status))
		c.publishRun(ctx, r)
	}
	if r.Status == model.RunQueued {
		c.closeQueuedCancelInline(ctx, r.ID, c.rememberBearer(ctx, userID))
	}
	return c.awaitCancel(ctx, r.ID)
}

// closeQueuedCancelInline 抢得到行租约（此刻没有派发器 / 排队推进拿着它）就由本次请求收尾；抢不到交给持有方。
func (c *core) closeQueuedCancelInline(ctx context.Context, id int64, bearer string) {
	owner := lease.Owner()
	ok, err := c.s.Repo.Run.AcquireLease(ctx, id, owner, runLeaseTTL)
	if err != nil || !ok {
		if err != nil {
			logx.WithContext(ctx).Errorf("canvas: run %d 取消收尾抢租约失败，交给派发器 / 排队推进: %v", id, err)
		}
		return
	}
	var cur *model.NodeRun
	if cur, err = c.s.Repo.Run.Get(ctx, id); err == nil && cur != nil && cur.Status == model.RunQueued && cur.CancelRequest == model.CancelRequested {
		c.closeQueuedCancel(ctx, cur, bearer)
	}
	rctx, cancel := detached(ctx)
	if rerr := c.s.Repo.Run.ReleaseLease(rctx, id, owner); rerr != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 释放租约失败（到期自动失效）: %v", id, rerr)
	}
	cancel()
	c.handOff(cur) // 有调用已发出、转运行中：交给长轮询协程
}

// awaitCancel 等取消请求的结局（见 requestCancel 第 2 步）。
func (c *core) awaitCancel(ctx context.Context, id int64) (*canvas.RunResult, bool, error) {
	deadline := c.now().Add(c.cancelWait)
	for {
		cur, err := c.s.Repo.Run.Get(ctx, id)
		if err != nil {
			return nil, true, err
		}
		if cur == nil {
			return nil, true, xerr.NotFoundf("执行记录")
		}
		switch {
		case model.RunFinal(cur.Status):
			c.s.TaskSync.Kick()
			return c.toResult(ctx, cur), true, nil
		case cur.Status == model.RunRunning && cur.CancelRequest == model.CancelRefused:
			return nil, true, errCancelVendorSent()
		case cur.Status == model.RunRunning:
			return c.toResult(ctx, cur), true, nil
		}
		if !c.now().Before(deadline) {
			return c.toResult(ctx, cur), true, nil
		}
		select {
		case <-ctx.Done():
			return c.toResult(ctx, cur), true, nil
		case <-time.After(cancelPollInterval):
		}
	}
}

func (c *core) ListRuns(ctx context.Context, in *canvas.ListRunsReq) (*canvas.ListRunsResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	c.rememberBearer(ctx, in.UserId)
	limit := int(in.Limit)
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	list, err := c.s.Repo.Run.ListByCanvas(ctx, in.UserId, in.CanvasId, in.NodeId, limit)
	if err != nil {
		return nil, err
	}
	out := &canvas.ListRunsResp{List: make([]*canvas.RunResult, 0, len(list))}
	for _, r := range list {
		out.List = append(out.List, c.toResult(ctx, r))
	}
	return out, nil
}

// SelectCandidate 选中某次成功 run 的第 index 张候选，写回 run.selected_index（用户数据，GET 画布回填以它为准）。
func (c *core) SelectCandidate(ctx context.Context, in *canvas.SelectCandidateReq) (*canvas.RunResult, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.RunId) == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少 run_id")
	}
	cv, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.CanvasId)
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, xerr.NotFoundf("画布")
	}
	r, err := c.s.Repo.Run.GetByCanvasRun(ctx, in.UserId, cv.ID, in.NodeId, strings.TrimSpace(in.RunId))
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, xerr.NotFoundf("执行记录")
	}
	if r.Status != model.RunSucceeded {
		return nil, xerr.New(xerr.Conflict, "该次执行没有成功，不能选择候选")
	}
	n := len(r.CandidateList())
	if in.Index < 0 || int(in.Index) >= n {
		return nil, xerr.Param(fmt.Sprintf("index 取值 0–%d", max(0, n-1)))
	}
	if r.SelectedIndex != in.Index {
		if err := c.s.Repo.Run.SetSelected(ctx, r.ID, in.Index); err != nil {
			return nil, err
		}
		r.SelectedIndex = in.Index
		c.publishRun(ctx, r)
	}
	return c.toResult(ctx, r), nil
}

// maxPointsOf 回给前端的「最多扣多少点」：冻结了才有（= 冻结的 credits）。
func maxPointsOf(settleMode string, points int64) int64 {
	if settleMode == "" {
		return 0
	}
	return points
}
