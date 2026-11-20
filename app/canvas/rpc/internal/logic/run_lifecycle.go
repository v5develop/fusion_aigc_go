package logic

// run 生命周期里与「钱」和「上游调用」相关的收尾：
//
//	提交中 ──Hold 成功，一次 CAS 写 billing_status + points──▶ 排队 ──提交后端──▶ 运行中 ──▶ 终态
//	   │                                                       │
//	   └─Hold 失败 → 失败（未冻结，不退款）                    └─排队超时 / 画布已删 / 提交失败 → 失败或取消（退款 + 取消已拿到的上游调用）
//
// 冻结、结算、退款与取消上游一律用 context.WithoutCancel(ctx) + 独立超时：前端断开或进程退出时 ctx 被取消，
// 不能让「已冻结未写库」「已成功未结算」这类半截状态因为 ctx 取消而留下。

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/taskevent"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// settleTimeout 冻结 / 结算 / 退款 / 取消上游的独立超时。
	settleTimeout = 60 * time.Second
	// submittingTTL 提交中超过这么久（Hold 自己最多 60s）视为提交方已崩溃：退回可能已冻结的点数并置失败。
	submittingTTL = 5 * time.Minute
)

var errActiveOnNode = xerr.New(xerr.Conflict, "该节点上一次执行还未结束")

// errNoTaskID run 还没有 task_id：幂等键 / 计费请求号无从生成，绝不能退化成共用键。
var errNoTaskID = errors.New("canvas: run 缺少 task_id，不能生成幂等键")

// detached 不随调用方取消的上下文（保留 trace 等值），带独立超时。
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}

// runKey 本 run 对外部系统的幂等键 canvas-<task_id>（Java Idempotency-Key / studio request_id / 计费请求号同源）。
func runKey(run *model.NodeRun) (string, error) {
	if !run.TaskID.Valid || run.TaskID.String == "" {
		return "", errNoTaskID
	}
	return BillingPrefix + run.TaskID.String, nil
}

// insertRun 落库（提交中）并在同一事务里回填 task_id / billing_request_id。
func (c *core) insertRun(ctx context.Context, run *model.NodeRun, sp *runSpec) error {
	prefix := c.s.Config.TaskIDPrefix
	return c.s.Repo.InsertRun(ctx, run, func(id int64) (string, string) {
		taskID := aigc.NewLifeTaskID(prefix, taskcenter.Ref(prefix, id), taskIDStage)
		billReq := ""
		if sp.Backend == model.BackendGateway {
			billReq = BillingPrefix + taskID
		}
		return taskID, billReq
	})
}

// holdAndQueue 提交中 → 冻结（gateway 后端）→ 一次 CAS 写 billing_status / points 并转排队。
//   - Hold 失败：置失败（没冻结，不退款；兜底调一次 Fail，防 Hold 超时返回但库里已落单）。
//   - CAS 没抢到（提交中被取消 / 强制失败）：退回刚冻结的点数。
//   - CAS 写库出错：不退款，交给轮询器的提交中兜底（5 分钟后退回并置失败），避免「库里已转排队却被退了款」。
func (c *core) holdAndQueue(ctx context.Context, run *model.NodeRun, sp *runSpec) error {
	bctx, cancel := detached(ctx)
	defer cancel()
	log := logx.WithContext(ctx)
	set := map[string]any{}
	bill := run.BillingStatus
	if sp.Backend == model.BackendGateway {
		c.evStart(bctx, run, stFreeze, 0, runAttrs(run, taskevent.Attrs{"entry_code": sp.EntryCode}))
		r, err := c.s.Billing.Hold(bctx, c.charge(run.UserID, run.BillingRequestID.String, sp))
		if err != nil {
			code, msg, ok := xerr.Parse(err)
			if !ok {
				log.Errorf("canvas: run %d 冻结失败: %v", run.ID, err)
				code, msg = xerr.ServerError, "暂时无法计费，请稍后重试"
			}
			c.evEnd(bctx, run, stFreeze, 0, taskevent.StatusFailed, taskevent.Fail{Class: "BILLING", Code: "BILLING_REJECTED", Check: "canvas.billing_hold", Message: msg}, runAttrs(run, nil))
			c.refundTrace(bctx, run, "冻结失败："+msg)
			c.endRun(bctx, run, model.RunSubmitting, model.RunFailed, "BILLING_REJECTED", msg, false, false)
			return xerr.New(code, msg)
		}
		bill = billingStatusOf(c.s.Billing.Mode(), r)
		set["billing_status"] = bill
		if r != nil {
			set["points"] = r.PricePoints
			run.Points = r.PricePoints
			sp.SettleMode = c.s.Billing.SettleModeOf(bctx, r)
		}
		c.evEnd(bctx, run, stFreeze, 0, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, taskevent.Attrs{"points": run.Points, "billing_status": bill, "settle_mode": sp.SettleMode}))
	}
	ok, err := c.s.Repo.Run.Transit(bctx, run.ID, model.RunSubmitting, model.RunQueued, set)
	if err != nil {
		log.Errorf("canvas: run %d 冻结后转排队写库失败（由提交中兜底处理）: %v", run.ID, err)
		return xerr.New(xerr.ServerError, "提交失败，请稍后重试")
	}
	if !ok {
		if bill == model.BillHeld {
			c.refundTrace(bctx, run, "提交中已取消")
		}
		return xerr.New(xerr.Conflict, "执行已被取消")
	}
	run.Status, run.BillingStatus = model.RunQueued, bill
	c.publishRun(bctx, run) // 受理：另一个窗口 / 智能体触发的生成，编辑器据此开始轮询该节点
	return nil
}

// advanceSubmitting 提交中兜底：创建超过 submittingTTL 仍提交中 → CAS 置失败，有计费请求号的退回（无单时是空操作）。
func (c *core) advanceSubmitting(ctx context.Context) {
	list, err := c.s.Repo.Run.ListStale(ctx, model.RunSubmitting, c.now().Add(-submittingTTL), c.s.Config.Poll.BatchSize)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 查提交中 run 失败: %v", err)
		return
	}
	for _, run := range list {
		bctx, cancel := detached(ctx)
		set := map[string]any{"error_code": "SUBMIT_TIMEOUT", "error_reason": "提交超时，点数已退回"}
		if run.BillingRequestID.Valid {
			set["billing_status"], set["points"] = model.BillRefunded, 0
		}
		ok, err := c.s.Repo.Run.Transit(bctx, run.ID, model.RunSubmitting, model.RunFailed, set)
		if err != nil {
			logx.WithContext(ctx).Errorf("canvas: run %d 提交中兜底置失败出错: %v", run.ID, err)
		} else if ok {
			logx.WithContext(ctx).Errorf("canvas: run %d 提交中超过 %s，已置失败并退回冻结", run.ID, submittingTTL)
			c.traceEnded(bctx, run, model.RunSubmitting, "SUBMIT_TIMEOUT", "提交超时，点数已退回", run.BillingRequestID.Valid)
			if run.BillingRequestID.Valid {
				c.refundTrace(bctx, run, "提交超时")
			}
			run.Status = model.RunFailed
			c.publishRun(bctx, run)
		}
		cancel()
	}
}

// cancelCalls 尽力取消 gateway 调用（孤儿调用 / 超时 / 运营强制失败 / 排队回收等系统或运营决定的路径），
// 返回被网关以「已发给供应商」拒绝取消的调用。
//
// 网关对已发给供应商的调用回 409 CANCEL_NOT_ALLOWED（check=cancel.vendor_sent）：调用状态不变、继续运行。这里只打 INFO
// 留证据，不当错误、不重试（重试也只会再被拒）。超时路径据返回值不失败、等被拒调用到终态（见 gateway_timeout.go）；
// 运营强制失败 / 提交失败孤儿等路径的结束 / 退款决定已经做出，被拒的调用之后若成功，产物和供应商费用由网关记账
// （late 结果），画布侧不会再收尾——与网关加这条契约之前（调用被置 CANCELLED、产物另存）的结局相同。
func (c *core) cancelCalls(ctx context.Context, runID int64, ids []string, why string) (refused []string) {
	if len(ids) == 0 || !c.s.Gateway.Enabled() {
		return nil
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	for _, id := range ids {
		_, err := c.s.Gateway.Cancel(bctx, id)
		switch {
		case err == nil:
		case aigateway.IsCancelNotAllowed(err):
			refused = append(refused, id)
			// 超时等待期内每个轮询 tick 都会再取消一次同一条调用：同一条只记第一次，不刷屏
			if cancelRefusedLogged.first(id) {
				logx.WithContext(ctx).Infof("canvas: run %d %s，gateway 调用 %s 已发给供应商，网关拒绝取消，调用继续运行 (check=%s)",
					runID, why, id, aigateway.CheckCancelVendorSent)
			}
		default:
			logx.WithContext(ctx).Infof("canvas: run %d %s，取消 gateway 调用 %s: %v", runID, why, id, err)
		}
	}
	return refused
}

// onceSet 有上限的「只记一次」集合（满了整体清空，最坏多打几行日志）。
type onceSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func (s *onceSet) first(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[k]; ok {
		return false
	}
	if s.m == nil || len(s.m) >= reasonCacheMax {
		s.m = map[string]struct{}{}
	}
	s.m[k] = struct{}{}
	return true
}

// cancelRefusedLogged 已打过「网关拒绝取消」日志的调用。
var cancelRefusedLogged = &onceSet{}

// msgCancelVendorSent 用户取消被拒的文案（画布 / studio 同一句）。
const msgCancelVendorSent = "已开始生成，无法取消，完成后按实际用量结算"

func errCancelVendorSent() error {
	return xerr.NewReason(xerr.Conflict, xerr.ReasonCancelVendorSent, msgCancelVendorSent)
}

// callVendorSent 这条调用是否已经（可能）发给供应商、或已有结果：是则用户不能取消。
//
//   - 终态 SUCCEEDED（结果已出，由轮询器照常交付结算）/ UNKNOWN（可能已扣费，按待对账处理）→ 是；FAILED / CANCELLED → 否；
//   - 未终态：网关 vendor_accepted、受理后的阶段 / 尝试（callAccepted），或正在发送（phase=SENDING、本轮未结束的尝试
//     处于 SENDING 或已带 sent_at）→ 是。
//
// 只用于预检、宁严勿松：判成「已发送」最多是让用户少取消一次；真正的裁决在网关（Cancel 回 409 CANCEL_NOT_ALLOWED）。
func callVendorSent(call *aigateway.Call) bool {
	if call == nil {
		return false
	}
	if call.IsTerminal() {
		return call.Status == aigateway.StatusSucceeded || call.Status == aigateway.StatusUnknown
	}
	if call.VendorAccepted || callAccepted(call) || call.Phase == "SENDING" {
		return true
	}
	for _, a := range call.Attempts {
		if call.RunNo > 0 && a.RunNo > 0 && a.RunNo != call.RunNo {
			continue
		}
		if attemptEnded(a.State) {
			continue
		}
		if a.State == "SENDING" || a.SentAt != nil {
			return true
		}
	}
	return false
}

// userCancelGatewayCalls 用户取消一个已有 gateway 调用的 run：**先**取消上游，全部取消成功（或本就已失败 / 已取消）
// 才返回 nil，由调用方结束 run 并退款。产品决策：请求一旦发给第三方供应商（受理即扣费、Atlas 等没有取消接口），
// 用户就不能再取消，任务照常完成、交付并按实际用量结算；只有还没发出去的可以取消。
//
// 两步走，防止多调用 run（count>1，幂等键 canvas:<task_id>:<i>）被取消一半：
//  1. 预检：BatchGet 读全部调用，任一已发送 / 已有结果（callVendorSent）→ 一条都不取消，直接拒绝（errCancelVendorSent）。
//     读不到（网关不可用）→ 不取消、不结束，回可重试错误：不知道是否已发送时不退款。
//  2. 逐个取消未终态的调用。预检之后到取消之间某条刚被发出去（竞态）→ 网关回 409 CANCEL_NOT_ALLOWED；取消接口返回
//     SUCCEEDED / UNKNOWN 同理。此时前面已取消的几条是**未发送**的（零成本），run 保持运行中、不退款，拒绝用户取消，
//     交给轮询器按既有的部分成功语义收尾（ClassifyGateway：成功 + 已取消 → 成功，actual=成功张数，按成功张数 / 实报成本
//     结算并退回差额；没有任何成功 → 按失败 / 已取消退款；含 UNKNOWN → 待对账不退款）。这样不会多收（只为成功的出图付费），
//     也不会少收（发出去的调用成功了照常结算，而不是像以前先全退再丢产物）。其它取消错误（网关暂时不可用等）同样保持运行中、
//     回可重试错误。调用不存在（404）视为无需取消。
func (c *core) userCancelGatewayCalls(ctx context.Context, run *model.NodeRun, ids []string) error {
	log := logx.WithContext(ctx)
	bctx, cancel := detached(ctx)
	defer cancel()
	calls := map[string]*aigateway.Call{}
	for i := 0; i < len(ids); i += aigateway.MaxBatchGet {
		got, err := c.s.Gateway.BatchGet(bctx, ids[i:min(len(ids), i+aigateway.MaxBatchGet)])
		if err != nil {
			log.Errorf("canvas: run %d 用户取消，查 gateway 调用失败，不取消、不结束 (check=canvas.cancel_precheck): %v", run.ID, err)
			return xerr.New(xerr.ThirdPartyError, "暂时无法取消，请稍后重试")
		}
		for j := range got {
			calls[got[j].ID] = &got[j]
		}
	}
	for _, id := range ids {
		if callVendorSent(calls[id]) {
			log.Infof("canvas: run %d 用户取消被拒：gateway 调用 %s 已发给供应商（状态 %s），任务照常完成、按实际用量结算 (check=%s)",
				run.ID, id, calls[id].Status, aigateway.CheckCancelVendorSent)
			return errCancelVendorSent()
		}
	}
	var refused, failed []string
	for _, id := range ids {
		if x := calls[id]; x != nil && x.IsTerminal() { // 已失败 / 已取消：不用再取消
			continue
		}
		got, err := c.s.Gateway.Cancel(bctx, id)
		switch {
		case err == nil && got != nil && (got.Status == aigateway.StatusSucceeded || got.Status == aigateway.StatusUnknown):
			refused = append(refused, id) // 取消前一刻已出结果：照常交付结算
		case err == nil, errors.Is(err, aigateway.ErrNotFound):
		case aigateway.IsCancelNotAllowed(err):
			refused = append(refused, id)
		default:
			log.Errorf("canvas: run %d 用户取消，取消 gateway 调用 %s 失败: %v", run.ID, id, err)
			failed = append(failed, id)
		}
	}
	if len(refused) > 0 {
		log.Infof("canvas: run %d 用户取消被拒：调用 %s 在预检后已发给供应商 / 已出结果，run 继续运行，未发送的已取消（零成本），由轮询器按部分成功收尾 (check=%s)",
			run.ID, strings.Join(refused, ","), aigateway.CheckCancelVendorSent)
		return errCancelVendorSent()
	}
	if len(failed) > 0 {
		return xerr.New(xerr.ThirdPartyError, "暂时无法取消，请稍后重试")
	}
	return nil
}

// cancelUnrecordedCalls 置状态的 CAS 没抢到时收尾本次提交拿到的 gateway 调用：重读 run 再决定取消哪些。
//
// 没抢到说明 run 已被别处推进：取消 / 强制失败 / 超时（钱已由那条路径处理），或——提交耗时超过行租约（3 分钟）后 run 被别的实例
// 接手，它用同一幂等键（canvas:<task_id>:<i>，与轮次无关）重提拿回同一批调用、正在或即将写进 backend_ref 跟踪（用户的点数还冻结着）。
//   - run 还没到终态（排队 / 运行中）：归接手方。一个都不取消——接手方可能已拿到、但还没写进 backend_ref，按「没记录」取消会误杀；
//     接手方按同一幂等键会拿回这批调用中它提交到的每一条，它自己失败时由它的失败路径（cancelCalls + reclaimQueuedCalls）收尾。
//   - run 已到终态 / 行已不在：run 行记录在案的调用归记录它的那条路径（成功的不能取消；取消 / 超时路径按 callIDs(run) 自己取消），
//     只取消没记录的（真正的孤儿）。
//
// 重读失败时一个都不取消：宁可留一个由网关 deadline 兜底收尾的孤儿调用，也不误杀别人在跟踪的调用；打 ERROR 交排查。
func (c *core) cancelUnrecordedCalls(ctx context.Context, runID int64, ids []string, why string) {
	if len(ids) == 0 || !c.s.Gateway.Enabled() {
		return
	}
	rctx, cancel := detached(ctx)
	cur, err := c.s.Repo.Run.Get(rctx, runID)
	cancel()
	if err != nil {
		logx.WithContext(ctx).Errorw(fmt.Sprintf("canvas: run %d %s，重读 run 失败，不取消本次拿到的调用（可能已被别的实例接手在跟踪） (check=canvas.submit_orphan_calls)", runID, why),
			logx.Field("calls", strings.Join(ids, ",")), logx.Field("err", err.Error()))
		return
	}
	if cur != nil && !model.RunFinal(cur.Status) {
		logx.WithContext(ctx).Infof("canvas: run %d %s，run 仍在进行（状态 %d，已被别的实例接手），本次拿到的调用 %s 归接手方，不取消",
			runID, why, cur.Status, strings.Join(ids, ","))
		return
	}
	recorded := map[string]bool{}
	if cur != nil {
		for _, id := range callIDs(cur) {
			recorded[id] = true
		}
	}
	var orphans, kept []string
	for _, id := range ids {
		if recorded[id] {
			kept = append(kept, id)
		} else {
			orphans = append(orphans, id)
		}
	}
	if len(kept) > 0 {
		logx.WithContext(ctx).Infof("canvas: run %d %s，调用 %s 已记录在 run 上（别的实例接手在跟踪），不取消", runID, why, strings.Join(kept, ","))
	}
	if refused := c.cancelCalls(ctx, runID, orphans, why); len(refused) > 0 {
		// 提交期间 run 已被取消 / 强制失败 / 超时（点数已由那条路径退回），而本次拿到的调用已发给供应商、网关拒绝取消：
		// 供应商会扣费、产物不交付。run 已到终态且已退款，不能恢复成运行中（用户已看到取消、退款已发出），只记这笔成本待核对。
		st := int8(-1)
		if cur != nil {
			st = cur.Status
		}
		logx.WithContext(ctx).Errorw(fmt.Sprintf("canvas: run %d %s，run 已结束（状态 %d）但本次拿到的调用已发给供应商、网关拒绝取消，供应商成本由平台承担 (check=canvas.orphan_call_vendor_sent)", runID, why, st),
			logx.Field("calls", strings.Join(refused, ",")), logx.Field("gateway_check", aigateway.CheckCancelVendorSent))
	}
}

// cancelStudio 尽力取消 studio 批次。
func (c *core) cancelStudio(ctx context.Context, run *model.NodeRun, ref, why string) {
	bid, _ := strconv.ParseInt(ref, 10, 64)
	if c.s.Studio == nil || bid <= 0 {
		return
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	if _, err := c.s.Studio.CancelBatch(bctx, &studio.BatchUserReq{BatchId: bid, UserId: run.UserID}); err != nil {
		logx.WithContext(ctx).Infof("canvas: run %d %s，取消 studio 批次 %d: %v", run.ID, why, bid, err)
	}
}

// cancelUpstream 停止后端执行：gateway 取消全部调用；studio 取消批次。Java 后端不经这里：运行中的 Java run 由 stopJava
// 先调 Java 取消接口再按结果置终态（见 java_cancel.go），排队 / 提交中的还没有 Java 任务。
func (c *core) cancelUpstream(ctx context.Context, run *model.NodeRun, why string) {
	switch run.Backend {
	case model.BackendGateway:
		c.cancelCalls(ctx, run.ID, callIDs(run), why)
	case model.BackendStudio:
		c.cancelStudio(ctx, run, run.BackendRef.String, why)
	}
}

// reclaimQueuedCalls 排队超时 / 画布已删的 gateway run：此前提交过（attempt>0）但 backend_ref 没写上时，
// 用同一幂等键再提交一次取回调用 id（同键同体返回原调用，没有则新建），随即取消，避免孤儿调用继续烧钱。
func (c *core) reclaimQueuedCalls(ctx context.Context, run *model.NodeRun, why string) {
	if run.Backend != model.BackendGateway || run.Attempt <= 0 || !c.s.Gateway.Enabled() {
		return
	}
	sp, err := c.specFromRun(ctx, run)
	if err != nil {
		return
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	reqs, err := c.gatewayRequests(bctx, run, sp)
	if err != nil {
		logx.WithContext(ctx).Infof("canvas: run %d %s，重建请求失败，无法回收可能的孤儿调用: %v", run.ID, why, err)
		return
	}
	var ids []string
	for i, r := range reqs {
		key, err := gatewayIdem(run, i)
		if err != nil {
			return
		}
		call, err := c.s.Gateway.Submit(bctx, r, aigateway.SubmitOptions{IdempotencyKey: key, TraceID: c.traceIDOf(run.ID)})
		if err != nil {
			logx.WithContext(ctx).Infof("canvas: run %d %s，按幂等键 %s 取回调用失败: %v", run.ID, why, key, err)
			continue
		}
		ids = append(ids, call.ID)
	}
	if refused := c.cancelCalls(ctx, run.ID, ids, why); len(refused) > 0 {
		// 调用方都已先结束 run 并退款（用户取消 / 运营强制失败 / 排队超时 / 画布已删 / 提交失败），而按幂等键取回的调用
		// 已发给供应商、网关拒绝取消：供应商会扣费、产物不交付，只记这笔成本待核对（与 cancelUnrecordedCalls 一致）。
		logx.WithContext(ctx).Errorw(fmt.Sprintf("canvas: run %d %s，run 已结束（状态 %d）但按幂等键取回的调用已发给供应商、网关拒绝取消，供应商成本由平台承担 (check=canvas.orphan_call_vendor_sent)", run.ID, why, run.Status),
			logx.Field("calls", strings.Join(refused, ",")), logx.Field("gateway_check", aigateway.CheckCancelVendorSent))
	}
}

// reclaimUnrefCalls 取消 / 强制失败的 gateway run 提交过（attempt>0）但没有 backend_ref：此前的提交可能已建调用，
// 按幂等键取回并取消（有 backend_ref 的已由 cancelUpstream 取消）。
func (c *core) reclaimUnrefCalls(ctx context.Context, run *model.NodeRun, why string) {
	if len(callIDs(run)) > 0 {
		return
	}
	c.reclaimQueuedCalls(ctx, run, why)
}

// settle 成功后的确认扣费：Settle 返回错误（含 ErrGone：预留单已过期回收 / 已退）→ billing_status=unsettled + ERROR 日志
// 待对账；成功 → settled。此时 run 已是成功（先 CAS 再结算，避免取消与成功并发时重复扣费）。
//
// 载体按实际用量结算（ACTUAL_COST，如 GPT Image 2.6）时按 gateway 实报成本确认、退回差额，并把 run.points 改成实扣点数；
// calls 是本轮判定用的调用（可为 nil：重试结算时按 backend_ref 再查）。
func (c *core) settle(ctx context.Context, run *model.NodeRun, actual int, calls []*aigateway.Call) {
	bctx, cancel := detached(ctx)
	defer cancel()
	bill := model.BillSettled
	set := map[string]any{}
	// 每次结算尝试各占一个序号：失败后重试成功的那次不会被先到的失败行吞掉（start / end 共用同一序号）
	sseq := nowSeq()
	c.evStart(bctx, run, stSettle, sseq, runAttrs(run, taskevent.Attrs{"actual": actual}))
	st, err := c.s.Billing.SettleCost(bctx, run.UserID, run.BillingRequestID.String, actual, c.runCost(run, calls))
	if err != nil {
		bill = model.BillUnsettled
		logx.WithContext(ctx).Errorf("canvas: run %d 已交付但确认扣费失败（预留单 %s，标 unsettled 待对账）: %v",
			run.ID, run.BillingRequestID.String, err)
	} else if st != nil && st.ActualCost && !st.Already {
		set["points"] = st.Points
		run.Points = st.Points
	}
	set["billing_status"] = bill
	if bill == model.BillUnsettled {
		c.evEnd(bctx, run, stSettle, sseq, taskevent.StatusFailed, taskevent.Fail{Class: "BILLING", Code: "SETTLE_FAILED", Check: "canvas.settle", Message: traceErrMsg(err)}, runAttrs(run, taskevent.Attrs{"points": run.Points}))
	} else {
		c.evEnd(bctx, run, stSettle, sseq, taskevent.StatusOK, taskevent.Fail{}, runAttrs(run, taskevent.Attrs{"points": run.Points, "actual": actual}))
	}
	if err := c.s.Repo.Run.Update(bctx, run.ID, set); err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 写计费状态 %s 失败: %v", run.ID, bill, err)
	}
	run.BillingStatus = bill
}

// runCost 这个 run 全部 gateway 调用的实报成本（美元）：优先用本轮已查到的调用，缺了就按 backend_ref 批量再查一次。
// 只在载体是 ACTUAL_COST 时被调用。
func (c *core) runCost(run *model.NodeRun, calls []*aigateway.Call) billing.CostFunc {
	return func(ctx context.Context) (*big.Rat, bool) {
		ids := callIDs(run)
		if len(ids) == 0 {
			return nil, false
		}
		have := len(calls) == len(ids)
		for _, x := range calls {
			if x == nil || !x.Cost.Authoritative() {
				have = false // 调用级成本未定稿时再查一次，网关可能刚回填
			}
		}
		if !have {
			// 再查不了 / 查失败时仍用手上这批调用（调用级成本没定稿时可能逐个尝试都已实报，ReportedUSD 会合计；不全则不可用）
			fallback := func() (*big.Rat, bool) {
				if len(calls) != len(ids) {
					return nil, false
				}
				return aigateway.ReportedUSD(calls)
			}
			if c.s.Gateway == nil || !c.s.Gateway.Enabled() {
				return fallback()
			}
			got, err := c.s.Gateway.BatchGet(ctx, ids)
			if err != nil {
				logx.WithContext(ctx).Errorf("canvas: run %d 按实际用量结算查 gateway 调用失败: %v", run.ID, err)
				return fallback()
			}
			byID := make(map[string]*aigateway.Call, len(got))
			for i := range got {
				byID[got[i].ID] = &got[i]
			}
			calls = calls[:0:0]
			for _, id := range ids {
				calls = append(calls, byID[id])
			}
		}
		return aigateway.ReportedUSD(calls)
	}
}

// touch 本轮处理过、仍停在 status 的 run 排到队尾。
func (c *core) touch(ctx context.Context, list []*model.NodeRun, status int8) {
	ids := make([]int64, 0, len(list))
	for _, r := range list {
		if r.Status == status {
			ids = append(ids, r.ID)
		}
	}
	if err := c.s.Repo.Run.Touch(ctx, ids, status); err != nil {
		logx.WithContext(ctx).Errorf("canvas: 刷新 run updated_at 失败: %v", err)
	}
}

// goneCanvases 本批 run 所属画布里已删除的。
func (c *core) goneCanvases(ctx context.Context, list []*model.NodeRun) map[string]bool {
	seen := map[string]bool{}
	var ids []string
	for _, r := range list {
		if !seen[r.CanvasID] {
			seen[r.CanvasID] = true
			ids = append(ids, r.CanvasID)
		}
	}
	gone, err := c.s.Repo.Canvas.Gone(ctx, ids)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 查已删画布失败: %v", err)
		return nil
	}
	return gone
}
