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

	"github.com/uptrace/bun"
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

// advanceSubmitting 提交中兜底（Poller 每轮）：
//   - 冻结阶段（attempt=0）创建超过 submittingTTL 仍提交中 → CAS 置失败（已请求取消的置已取消），有计费请求号的退回
//     （无单时是空操作）。这一阶段从没向后端发过请求，退款不需要再确认。
//   - 派发阶段（attempt>0）超过 submittingTTL 没有动静、租约也已失效（派发器宕机 / 早已放手）→ 退回排队：没请求取消的
//     由排队推进按同一幂等键重提（拿回同一批调用，不重复提交）；已请求取消的就地按取消收尾——确认网关上没有已发出的调用
//     才退款，已发出的转运行中按实际结算（closeQueuedCancel → abortUnsent）。
func (c *core) advanceSubmitting(ctx context.Context) {
	c.advanceStaleHolding(ctx)
	c.advanceStaleDispatching(ctx)
}

func (c *core) advanceStaleHolding(ctx context.Context) {
	list, err := c.s.Repo.Run.ListStaleHolding(ctx, c.now().Add(-submittingTTL), c.s.Config.Poll.BatchSize)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 查提交中 run 失败: %v", err)
		return
	}
	for _, run := range list {
		bctx, cancel := detached(ctx)
		to, code, msg := model.RunFailed, "SUBMIT_TIMEOUT", "提交超时，点数已退回"
		if run.CancelRequest == model.CancelRequested {
			to, code, msg = model.RunCanceled, "CANCELED", msgUserCanceled
		}
		set := map[string]any{"error_code": code, "error_reason": msg}
		if run.BillingRequestID.Valid {
			set["billing_status"], set["points"] = model.BillRefunded, 0
		}
		ok, err := c.s.Repo.Run.Transit(bctx, run.ID, model.RunSubmitting, to, set)
		if err != nil {
			logx.WithContext(ctx).Errorf("canvas: run %d 提交中兜底置失败出错: %v", run.ID, err)
		} else if ok {
			logx.WithContext(ctx).Errorf("canvas: run %d 提交中（冻结阶段）超过 %s，已置%s并退回冻结", run.ID, submittingTTL, model.StatusText(to))
			c.traceEnded(bctx, run, model.RunSubmitting, code, msg, run.BillingRequestID.Valid)
			if run.BillingRequestID.Valid {
				c.refundTrace(bctx, run, "提交超时")
			}
			run.Status = to
			c.publishRun(bctx, run)
		}
		cancel()
	}
}

func (c *core) advanceStaleDispatching(ctx context.Context) {
	list, err := c.s.Repo.Run.ListStaleDispatching(ctx, c.now().Add(-submittingTTL), c.s.Config.Poll.BatchSize)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 查派发中 run 失败: %v", err)
		return
	}
	if len(list) == 0 {
		return
	}
	ls := c.leases()
	defer ls.Close()
	for _, run := range list {
		if !acquire(ctx, ls, run) {
			continue
		}
		if code, ok := strings.CutPrefix(run.ErrorCode.String, submitUnconfirmed); ok && run.Backend == model.BackendGateway {
			// 提交以永久错误结束、当时有提交结局未知：此刻（过了 submittingTTL）按幂等键查，404 才算没建单——
			// 都没发出 → 失败退款；有已发出的 → 转运行中按实际结算；仍无法确认 → 留着下一轮
			c.abortUnsent(ctx, run, model.RunSubmitting, runEnd{to: model.RunFailed, code: code, msg: run.ErrorReason.String, why: "提交失败（兜底确认）"}, nil, true)
			ls.Release(ctx, run.ID)
			c.handOff(run)
			continue
		}
		ok, err := c.s.Repo.Run.TransitAttempt(ctx, run.ID, model.RunSubmitting, model.RunQueued, run.Attempt, nil)
		switch {
		case err != nil:
			logx.WithContext(ctx).Errorf("canvas: run %d 派发中兜底退回排队出错: %v", run.ID, err)
		case ok:
			run.Status = model.RunQueued
			c.publishRun(ctx, run)
			logx.WithContext(ctx).Errorf("canvas: run %d 派发中超过 %s 无人处理（派发器宕机？），已退回排队 (check=canvas.dispatch_stale cancel_request=%d)",
				run.ID, submittingTTL, run.CancelRequest)
			if run.CancelRequest == model.CancelRequested {
				c.closeQueuedCancel(ctx, run, c.s.Bearers.Get(run.UserID))
			}
		}
		ls.Release(ctx, run.ID)
		c.handOff(run)
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
	refused, _ = c.cancelCallsVerdict(ctx, runID, ids, why)
	return refused
}

// cancelCallsVerdict 同 cancelCalls，另返回取消出错（网关暂时不可用等，不知道是否已取消）的调用：调用方据此不把「没确认」当「已取消」。
func (c *core) cancelCallsVerdict(ctx context.Context, runID int64, ids []string, why string) (refused, failed []string) {
	if len(ids) == 0 || !c.s.Gateway.Enabled() {
		return nil, nil
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	for _, id := range ids {
		_, err := c.s.Gateway.Cancel(bctx, id)
		switch {
		case err == nil, errors.Is(err, aigateway.ErrNotFound):
		case aigateway.IsCancelNotAllowed(err):
			refused = append(refused, id)
			// 超时等待期内每个轮询 tick 都会再取消一次同一条调用：同一条只记第一次，不刷屏
			if cancelRefusedLogged.first(id) {
				logx.WithContext(ctx).Infof("canvas: run %d %s，gateway 调用 %s 已发给供应商，网关拒绝取消，调用继续运行 (check=%s)",
					runID, why, id, aigateway.CheckCancelVendorSent)
			}
		default:
			failed = append(failed, id)
			logx.WithContext(ctx).Infof("canvas: run %d %s，取消 gateway 调用 %s: %v", runID, why, id, err)
		}
	}
	return refused, failed
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
	switch v, _ := c.cancelUnsent(ctx, run, ids, "用户取消"); v {
	case unsentSent:
		return errCancelVendorSent()
	case unsentUnknown:
		return xerr.New(xerr.ThirdPartyError, "暂时无法取消，请稍后重试")
	}
	return nil
}

// unsentVerdict cancelUnsent 的结论。
type unsentVerdict int

const (
	unsentConfirmed unsentVerdict = iota // 都没发出去：已取消 / 本就失败或取消 / 网关上不存在（或根本没有调用）
	unsentSent                           // 有调用已发给供应商 / 已出结果：网关拒绝取消，run 必须照常完成、按实际用量结算
	unsentUnknown                        // 暂时无法确认（读不到调用 / 取消出错）：不能退款
)

// cancelUnsent userCancelGatewayCalls 的两步（预检 + 逐个取消），给用户取消与排队 / 提交中收尾（abortUnsent）共用。
// 返回结论与被拒（已发送 / 已出结果）的调用。why 只用于日志。
func (c *core) cancelUnsent(ctx context.Context, run *model.NodeRun, ids []string, why string) (unsentVerdict, []string) {
	if len(ids) == 0 {
		return unsentConfirmed, nil
	}
	log := logx.WithContext(ctx)
	if !c.s.Gateway.Enabled() {
		log.Errorf("canvas: run %d %s，有 gateway 调用 %s 但网关未配置，无法确认是否已发出 (check=canvas.cancel_precheck)", run.ID, why, strings.Join(ids, ","))
		return unsentUnknown, nil
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	calls := map[string]*aigateway.Call{}
	for i := 0; i < len(ids); i += aigateway.MaxBatchGet {
		got, err := c.s.Gateway.BatchGet(bctx, ids[i:min(len(ids), i+aigateway.MaxBatchGet)])
		if err != nil {
			log.Errorf("canvas: run %d %s，查 gateway 调用失败，不取消、不结束 (check=canvas.cancel_precheck): %v", run.ID, why, err)
			return unsentUnknown, nil
		}
		for j := range got {
			calls[got[j].ID] = &got[j]
		}
	}
	for _, id := range ids {
		if callVendorSent(calls[id]) {
			log.Infof("canvas: run %d %s被拒：gateway 调用 %s 已发给供应商（状态 %s），任务照常完成、按实际用量结算 (check=%s)",
				run.ID, why, id, calls[id].Status, aigateway.CheckCancelVendorSent)
			return unsentSent, []string{id}
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
			log.Errorf("canvas: run %d %s，取消 gateway 调用 %s 失败: %v", run.ID, why, id, err)
			failed = append(failed, id)
		}
	}
	if len(refused) > 0 {
		log.Infof("canvas: run %d %s被拒：调用 %s 在预检后已发给供应商 / 已出结果，run 继续运行，未发送的已取消（零成本），由轮询器按部分成功收尾 (check=%s)",
			run.ID, why, strings.Join(refused, ","), aigateway.CheckCancelVendorSent)
		return unsentSent, refused
	}
	if len(failed) > 0 {
		return unsentUnknown, nil
	}
	return unsentConfirmed, nil
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
	c.cancelUnrecordedCallsOf(ctx, runID, cur, err, ids, why)
}

// cancelUnrecordedCallsOf 同 cancelUnrecordedCalls，run 已由调用方重读（cur / err 为重读结果）。
func (c *core) cancelUnrecordedCallsOf(ctx context.Context, runID int64, cur *model.NodeRun, err error, ids []string, why string) {
	if len(ids) == 0 || !c.s.Gateway.Enabled() {
		return
	}
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

// reclaimQueuedCalls 运营强制失败（先结束退款）的 gateway run：此前提交过（attempt>0）但 backend_ref 没写上时，
// 按幂等键取回调用 id 随即取消，避免孤儿调用继续烧钱（用户取消 / 排队超时 / 画布已删 / 提交失败改走 abortUnsent：先确认再退款）。
func (c *core) reclaimQueuedCalls(ctx context.Context, run *model.NodeRun, why string) {
	if run.Backend != model.BackendGateway || run.Attempt <= 0 || !c.s.Gateway.Enabled() {
		return
	}
	ids, _ := c.reclaimIDs(ctx, run, why)
	if refused := c.cancelCalls(ctx, run.ID, ids, why); len(refused) > 0 {
		// run 已先结束并退款（运营强制失败），而按幂等键取回的调用已发给供应商、网关拒绝取消：供应商会扣费、产物不交付，
		// 只记这笔成本待核对（与 cancelUnrecordedCalls 一致）。
		logx.WithContext(ctx).Errorw(fmt.Sprintf("canvas: run %d %s，run 已结束（状态 %d）但按幂等键取回的调用已发给供应商、网关拒绝取消，供应商成本由平台承担 (check=canvas.orphan_call_vendor_sent)", run.ID, why, run.Status),
			logx.Field("calls", strings.Join(refused, ",")), logx.Field("gateway_check", aigateway.CheckCancelVendorSent))
	}
}

// reclaimIDs 按幂等键（canvas:<task_id>:<i>）只查不建地取回本 run 在网关上已建的调用（GET /v1/calls?idempotency_key=），
// 按下标顺序。绝不用 Submit 同键重放「找回」：网关上没有时那会新建并立即派发，取消流程自己制造一次供应商调用，
// 之后取消被拒就把成本转给了用户。
//   - 404 → 该键在网关上不存在（没建过单），跳过；
//   - 其它错误（网络 / 5xx / 鉴权 / 冲突）→ ok=false：暂时无法确认，调用方不能据此退款，保持原状态等下一轮。
//
// 调用个数与提交一致：image 每张一个调用（requested_count 张），其余一个。
func (c *core) reclaimIDs(ctx context.Context, run *model.NodeRun, why string) (ids []string, ok bool) {
	if run.Backend != model.BackendGateway {
		return nil, true
	}
	log := logx.WithContext(ctx)
	if !c.s.Gateway.Enabled() {
		log.Errorf("canvas: run %d %s，网关未配置，无法确认此前的提交有没有建单 (check=canvas.reclaim_lookup)", run.ID, why)
		return nil, false
	}
	n := 1
	if run.Kind == model.KindImage {
		n = max(1, int(run.RequestedCount))
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	for i := range n {
		key, err := gatewayIdem(run, i)
		if err != nil {
			return ids, true // 没有 task_id 就生成不了幂等键，也就从没提交过
		}
		call, err := c.s.Gateway.GetByKey(bctx, key)
		switch {
		case err == nil:
			ids = append(ids, call.ID)
		case errors.Is(err, aigateway.ErrNotFound):
			// 没建过单
		default:
			log.Infof("canvas: run %d %s，按幂等键 %s 查调用失败，暂时无法确认，不退款、下一轮重试 (check=canvas.reclaim_lookup): %v", run.ID, why, key, err)
			return nil, false
		}
	}
	return ids, true
}

// runEnd abortUnsent 确认没发出去之后的结局。
type runEnd struct {
	to        int8
	code, msg string
	why       string // 日志用
	user      bool   // 用户取消：有调用已发出时把取消标记改成「被拒」（cancel_request=2）
}

// submitUnconfirmed 提交中 run 的 error_code 前缀：提交以永久错误结束但有提交结局未知，待兜底确认网关有无建单后再收尾
// （后接最终失败码，error_reason 为最终文案；提交中不对外展示 error_code）。
const submitUnconfirmed = "SUBMIT_UNCONFIRMED:"

// msgUserCanceled 用户取消的 error_reason。
const msgUserCanceled = "已取消执行"

var userCancelEnd = runEnd{to: model.RunCanceled, code: "CANCELED", msg: msgUserCanceled, why: "用户取消", user: true}

// abortUnsent 不再继续一个还没转运行中的 run（排队 / 提交中；调用方持有它：行租约 + 本次抢 run 的 CAS，或已请求取消）：
// 用户取消 / 排队超时 / 画布已删 / 提交失败。产品决策：退款只有一个主人，且只在确认网关上没有已发出的调用之后。
//
// known 是本次提交拿到的调用；reclaim=true 表示此前（或本次结局未知的）提交可能已建了没记下来的调用，按幂等键取回。然后：
//   - 全部确认没发出（已取消 / 不存在 / 从没提交）→ CAS from→to 并退款；
//   - 有调用已发给供应商（网关拒绝取消）→ 不退款，CAS from→运行中、backend_ref 记下全部调用，由轮询器按实际用量收尾
//     （ClassifyGateway 的部分成功语义）；用户取消时 cancel_request=2（接口 cancel_state=refused）；
//   - 暂时无法确认（网关不可用）→ 什么都不改，返回 false，由下一轮重试（排队推进 / 提交中兜底退回排队后再来）。
//
// 非 gateway 后端没有网关调用，直接 CAS from→to（Go 侧冻结的退款；Java / studio 自行计费）。
// 返回 run 是否已离开 from（已结束或已转运行中）。
func (c *core) abortUnsent(ctx context.Context, run *model.NodeRun, from int8, end runEnd, known []string, reclaim bool) bool {
	if run.Backend != model.BackendGateway {
		return c.endRun(ctx, run, from, end.to, end.code, end.msg, true, false)
	}
	log := logx.WithContext(ctx)
	if from == model.RunSubmitting {
		// 派发器提交得太久（行租约过期、兜底已退回排队）时 run 可能已被别的实例重新抢到（attempt 又 +1）在用同一幂等键提交：
		// 先确认仍归本方（状态 + attempt 与本方抢到时一致），否则只按 cancelUnrecordedCalls 口径处理本次拿到的调用
		rctx, cancel := detached(ctx)
		cur, err := c.s.Repo.Run.Get(rctx, run.ID)
		cancel()
		if err != nil || cur == nil || cur.Status != from || cur.Attempt != run.Attempt {
			c.cancelUnrecordedCallsOf(ctx, run.ID, cur, err, known, end.why)
			return false
		}
		run.CancelRequest = cur.CancelRequest
	}
	ids := known
	if reclaim {
		got, ok := c.reclaimIDs(ctx, run, end.why)
		if !ok {
			log.Infof("canvas: run %d %s，暂时无法确认网关上有没有已发出的调用，不结束、不退款，下一轮重试 (check=canvas.abort_unconfirmed)", run.ID, end.why)
			return false
		}
		ids = mergeIDs(got, known)
	}
	v, _ := c.cancelUnsent(ctx, run, ids, end.why)
	switch v {
	case unsentUnknown:
		log.Infof("canvas: run %d %s，暂时无法确认调用 %s 是否已发出，不结束、不退款，下一轮重试 (check=canvas.abort_unconfirmed)", run.ID, end.why, strings.Join(ids, ","))
		return false
	case unsentSent:
		// 清掉可能残留的待确认失败原因（SUBMIT_UNCONFIRMED:…）：run 接着按运行中跟踪
		set := map[string]any{"backend_ref": strings.Join(ids, ","), "progress": 5, "error_code": nil, "error_reason": nil}
		if end.user {
			set["cancel_request"] = model.CancelRefused
		} else {
			// 系统路径（提交失败 / 排队超时 / 画布已删）收尾时 run 若带着用户的取消标记，同样改成「被拒」，否则前端一直显示「取消中」
			set["cancel_request"] = bun.Safe(fmt.Sprintf("IF(cancel_request = %d, %d, cancel_request)", model.CancelRequested, model.CancelRefused))
		}
		bctx, cancel := detached(ctx)
		ok, err := c.s.Repo.Run.Transit(bctx, run.ID, from, model.RunRunning, set)
		cancel()
		if err != nil || !ok {
			// 写库失败（下一轮再来：同键取回同一批调用，仍被拒就再转）或 run 已被别处结束（运营强制失败，已退款）：
			// 后者已发出的调用成本由平台承担，只记这笔成本待核对
			log.Errorw(fmt.Sprintf("canvas: run %d %s，有调用已发给供应商（网关拒绝取消），转运行中跟踪失败 (check=canvas.orphan_call_vendor_sent)", run.ID, end.why),
				logx.Field("calls", strings.Join(ids, ",")), logx.Field("gateway_check", aigateway.CheckCancelVendorSent), logx.Field("err", fmt.Sprint(err)))
			return ok
		}
		log.Infof("canvas: run %d %s，有调用已发给供应商、网关拒绝取消：不退款，转运行中照常完成、按实际用量结算 (check=%s)", run.ID, end.why, aigateway.CheckCancelVendorSent)
		run.Status, run.BackendRef.String, run.BackendRef.Valid, run.Progress = model.RunRunning, strings.Join(ids, ","), true, 5
		if end.user || run.CancelRequest == model.CancelRequested {
			run.CancelRequest = model.CancelRefused
		}
		c.traceRunning(ctx, run, 0, len(ids))
		c.publishRun(ctx, run)
		return true
	}
	return c.endRun(ctx, run, from, end.to, end.code, end.msg, true, false)
}

// mergeIDs a 后接 b 里 a 没有的（保序去重）。
func mergeIDs(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := make(map[string]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// closeQueuedCancel 排队中、用户已请求取消的 run（调用方持有租约）按取消收尾：发过请求、可按幂等键找回的 Java 任务走 stopJava
// （由 Java 判断能否取消、由 Java 退款）；其余走 abortUnsent（gateway 先确认网关无已发出调用再退款）。
func (c *core) closeQueuedCancel(ctx context.Context, run *model.NodeRun, bearer string) {
	if javaLive(run) {
		c.stopJava(ctx, run, stopUser, msgUserCanceled, bearer, 0)
		return
	}
	c.abortUnsent(ctx, run, model.RunQueued, userCancelEnd, nil, run.Attempt > 0)
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
	} else if _, ok := set["points"]; ok {
		// 按实际用量结算改了点数：成功事件在结算前已推出（credits 还是冻结额），补推一次（失败只记日志）
		c.publishRunID(bctx, run.ID)
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
