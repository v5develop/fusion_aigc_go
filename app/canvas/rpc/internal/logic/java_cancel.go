package logic

// Java 兼容工具（tool_upscale / tool_outpaint / tool_inpaint）在途 run 的停止：通过 Java 现有取消接口
// POST /api/v1/creation/{taskId}/cancel 真实取消（Java 停上游并退款，Go 不退点数），拿不到有效登录态时退化为停止跟踪。
//
//	场景            Bearer 来源                          终态（取消成功）
//	用户 CancelRun  本次请求透传的 Bearer（无则缓存）    canceled
//	运营强制失败    BearerCache 里该用户最近的 token      failed（ADMIN_FORCE_FAIL）
//	Poller 超时     同上                                 failed（TIMEOUT）
//	画布已删        同上                                 canceled
//	提交失败        同上（dispatch 内联时为本次请求的）  failed（SUBMIT_FAILED）：已落 backend_request 的重提失败，Java 可能已建任务
//
//	Cancel 结果       处理
//	Cancelled         置终态，error_reason 注明「已请求 Java 取消，点数以 Java 终态为准」（code=0 只表示 Java 受理了取消）
//	AlreadyFinished   立刻按 taskUid 查一次：Java 已成功 → 用户取消 / 超时 / 画布已删按成功交付；强制失败照常置失败并注明 Java 已扣费；
//	                  Java 失败 / 已取消 → 置终态并注明；查不到或仍未终态 → 按 Transient
//	NotFound          视为已不存在：置终态（停止跟踪）
//	Unauthorized      清掉缓存的令牌，退化为停止跟踪（WARN + 计数）
//	Transient         run 保持原状态，在 error_code 记「待取消」标记（JAVA_CANCEL_PENDING:<场景>:<次数>:<上次尝试 unix 秒>，运行中不对外展示），
//	                  error_reason 记要落的文案；Poller 按上次尝试时间指数退避（默认 15s/30s/60s/2m/4m，Poll.JavaStopBackoffSeconds）
//	                  用缓存令牌重试，退避表用完（约 8 分钟）仍失败按停止跟踪处理
//
// 没有令牌：用户当次请求 / 运营强制失败（同步操作）立即退化；Poller 发起的（超时、画布已删、提交失败、待取消重试）
// 按 Transient 计次，给其它实例或用户下次请求补令牌的机会，退避表用完才退化。
//
// 状态迁移一律走 endRun / succeed 的 CAS（from=运行中），与 Poller 并发时只有一方生效；待取消标记也只在运行中时写入。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/javatool"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// javaStop 停止 Java run 的场景。
type javaStop byte

const (
	stopUser    javaStop = 'u' // 用户取消
	stopAdmin   javaStop = 'a' // 运营强制失败
	stopTimeout javaStop = 't' // Poller 超时
	stopGone    javaStop = 'g' // 画布已删
	stopSubmit  javaStop = 's' // 提交失败（已落 backend_request 的重提失败 / 次数用尽）
)

func (s javaStop) String() string {
	switch s {
	case stopUser:
		return "user_cancel"
	case stopAdmin:
		return "admin_force_fail"
	case stopTimeout:
		return "timeout"
	case stopGone:
		return "canvas_deleted"
	case stopSubmit:
		return "submit_failed"
	}
	return "unknown"
}

// target 场景的终态与失败码。
func (s javaStop) target() (int8, string) {
	switch s {
	case stopAdmin:
		return model.RunFailed, "ADMIN_FORCE_FAIL"
	case stopTimeout:
		return model.RunFailed, "TIMEOUT"
	case stopSubmit:
		return model.RunFailed, "SUBMIT_FAILED"
	}
	return model.RunCanceled, "CANCELED"
}

// background 由 Poller / dispatch 发起（不是用户或运营的同步操作）：缺令牌时计次重试而不是立即退化。
func (s javaStop) background() bool {
	return s == stopTimeout || s == stopGone || s == stopSubmit
}

// defaultJavaStopBackoff 停止 Java 任务暂时失败后的重试退避（第 n 次失败后等第 n 项；共 len+1 次尝试，约 8 分钟）。
var defaultJavaStopBackoff = []int{15, 30, 60, 120, 240}

const (
	// javaCancelPending 运行中 run 的 error_code 前缀：已决定取消、等 Poller 重试（运行中不对外展示 error_code）。
	javaCancelPending = "JAVA_CANCEL_PENDING:"
)

// error_reason 后缀。
const (
	sufJavaCancelled  = "；已请求 Java 取消，点数以 Java 终态为准"
	sufJavaNoLogin    = "；Java 任务未能取消（无有效登录态），可能仍会执行并扣费"
	sufJavaRetryOut   = "；Java 任务未能取消（取消接口多次失败），可能仍会执行并扣费"
	sufJavaNotFound   = "；Java 任务已不存在，已停止跟踪"
	sufJavaSucceeded  = "；Java 任务已成功并已扣费"
	sufJavaEndedFmt   = "；Java 任务已结束（%s）"
	sufJavaNoRef      = "；无法找回 Java 任务号，Java 任务未能取消，可能仍会执行并扣费"
	sufJavaNoTask     = "；Java 未建任务"
	javaDegradedCheck = "canvas.java_cancel_degraded"
)

// javaCancelDegraded 退化为「停止跟踪」的次数（Java 任务可能仍在执行、扣费），供运营到 Java 后台手动取消。
var javaCancelDegraded = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "canvas",
	Subsystem: "java_cancel",
	Name:      "degraded_total",
	Help:      "Java 工具 run 未能真实取消、退化为停止跟踪的次数",
	Labels:    []string{"scene", "reason"},
})

// pendingJavaStop 解析 run 的待取消标记。
func pendingJavaStop(run *model.NodeRun) (mode javaStop, tries int, ok bool) {
	mode, tries, _, ok = parsePendingJavaStop(run)
	return
}

// parsePendingJavaStop 解析待取消标记 JAVA_CANCEL_PENDING:<场景>:<次数>[:<上次尝试 unix 秒>]（旧格式没有时间戳，at 为零值）。
func parsePendingJavaStop(run *model.NodeRun) (mode javaStop, tries int, at time.Time, ok bool) {
	code := run.ErrorCode.String
	if !run.ErrorCode.Valid || !strings.HasPrefix(code, javaCancelPending) {
		return 0, 0, time.Time{}, false
	}
	parts := strings.Split(strings.TrimPrefix(code, javaCancelPending), ":")
	if (len(parts) != 2 && len(parts) != 3) || len(parts[0]) != 1 {
		return 0, 0, time.Time{}, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, time.Time{}, false
	}
	if len(parts) == 3 {
		sec, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return 0, 0, time.Time{}, false
		}
		at = time.Unix(sec, 0)
	}
	switch m := javaStop(parts[0][0]); m {
	case stopUser, stopAdmin, stopTimeout, stopGone, stopSubmit:
		return m, n, at, true
	}
	return 0, 0, time.Time{}, false
}

// javaStopBackoff 重试退避表（配置 Poll.JavaStopBackoffSeconds，未配用默认）。
func (c *core) javaStopBackoff() []time.Duration {
	secs := c.s.Config.Poll.JavaStopBackoffSeconds
	if len(secs) == 0 {
		secs = defaultJavaStopBackoff
	}
	out := make([]time.Duration, 0, len(secs))
	for _, s := range secs {
		out = append(out, time.Duration(max(s, 1))*time.Second)
	}
	return out
}

// javaStopMaxTries 停止 Java 任务最多尝试几次（含首次）：退避表每项对应一次重试。
func (c *core) javaStopMaxTries() int { return len(c.javaStopBackoff()) + 1 }

// javaStopDue 待取消标记是否到了下次重试时间：上次尝试时间 + 第 tries 项退避。没有时间戳（旧标记）视为已到。
func (c *core) javaStopDue(run *model.NodeRun) bool {
	_, tries, at, ok := parsePendingJavaStop(run)
	if !ok || at.IsZero() || tries <= 0 {
		return true
	}
	b := c.javaStopBackoff()
	return !c.now().Before(at.Add(b[min(tries, len(b))-1]))
}

// retryPendingJavaStop Poller 推进带待取消标记的 run：到了重试时间就再试一次停止。返回是否带标记（带标记的 run 本轮到此为止）。
func (c *core) retryPendingJavaStop(ctx context.Context, run *model.NodeRun, bearer string) bool {
	mode, tries, ok := pendingJavaStop(run)
	if !ok {
		return false
	}
	if c.javaStopDue(run) {
		c.stopJava(ctx, run, mode, firstNonEmpty(run.ErrorReason.String, "已取消执行"), bearer, tries)
	}
	return true
}

// stopJava 停止一个 Java 后端、未结束的 run。base 为终态文案主体（如「已取消执行」「运营已终止该任务：…」），
// tries 为此前已尝试取消的次数。返回 (是否已进终态, 是否已登记待重试)；两者都为 false 表示 CAS 落空（状态已被别处改变）。
func (c *core) stopJava(ctx context.Context, run *model.NodeRun, mode javaStop, base, bearer string, tries int) (ended, pending bool) {
	to, code := mode.target()
	log := logx.WithContext(ctx)
	if !javaLive(run) {
		// 没有 Java 任务：还没发过请求（提交中 / 排队且无 backend_request），直接置终态
		// （提交期间被抢先结束的孤儿任务由 dispatch 取消）
		return c.endRun(ctx, run, run.Status, to, code, base, true, false), false
	}
	if strings.TrimSpace(bearer) == "" {
		if mode.background() || tries > 0 {
			// Poller 发起的（非用户 / 运营当次请求）：计次等令牌（其它实例或用户下次请求会补进缓存），退避表用完才退化
			return c.retryJavaStop(ctx, run, mode, base, tries, javatool.ErrNoBearer)
		}
		return c.degradeJava(ctx, run, mode, base, "no_bearer", sufJavaNoLogin), false
	}
	var err error
	if run.Status == model.RunQueued {
		// 发过请求但 backend_ref 没写上（提交暂时失败）：同键逐字节重放找回 taskUid（见 java_request.go）。
		// Java 若没收到首次请求，这次会新建任务并冻结点数，随后立即取消、由 Java 退款。
		var rr recoverResult
		rr, err = c.recoverJavaRef(ctx, run, bearer)
		switch rr {
		case recoverOK:
		case recoverLost:
			return false, false
		case recoverUnauthorized:
			if c.s.Bearers.Get(run.UserID) == bearer {
				c.s.Bearers.Delete(run.UserID)
			}
			return c.degradeJava(ctx, run, mode, base, "unauthorized", sufJavaNoLogin), false
		case recoverNoTask:
			log.Infof("canvas: run %d（%s）重放被拒（%v）：同键在 Java 没有任务，直接收尾", run.ID, mode, err)
			return c.endRun(ctx, run, model.RunQueued, to, code, base+sufJavaNoTask, false, false), false
		case recoverUnrecoverable:
			log.Errorf("canvas: run %d（%s）按幂等键重放找回 Java 任务号被拒，无法取消: %v", run.ID, mode, err)
			return c.degradeJava(ctx, run, mode, base, "ref_unrecoverable", sufJavaNoRef), false
		default:
			return c.retryJavaStop(ctx, run, mode, base, tries, err)
		}
	}
	ref := strings.TrimSpace(run.BackendRef.String)
	bctx, cancel := detached(ctx)
	defer cancel()
	var out javatool.CancelOutcome
	out, err = c.s.Java.Cancel(bctx, bearer, ref)
	switch out {
	case javatool.CancelCancelled:
		log.Infof("canvas: run %d（%s）已取消 Java 任务 %s", run.ID, mode, ref)
		return c.endRun(ctx, run, model.RunRunning, to, code, base+sufJavaCancelled, false, false), false
	case javatool.CancelUnauthorized:
		if c.s.Bearers.Get(run.UserID) == bearer {
			c.s.Bearers.Delete(run.UserID) // 令牌已失效：不再拿它反复打 Java
		}
		return c.degradeJava(ctx, run, mode, base, "unauthorized", sufJavaNoLogin), false
	case javatool.CancelNotFound:
		log.Infof("canvas: run %d（%s）Java 任务 %s 不存在，停止跟踪: %v", run.ID, mode, ref, err)
		return c.endRun(ctx, run, model.RunRunning, to, code, base+sufJavaNotFound, false, false), false
	case javatool.CancelAlreadyFinished:
		t, gerr := c.s.Java.GetTask(bctx, bearer, ref)
		if gerr == nil && t.Terminal() {
			return c.finishedJava(ctx, run, mode, base, t), false
		}
		log.Infof("canvas: run %d（%s）Java 拒绝取消 %s（%v），但任务查询未见终态（%v），按暂时性重试", run.ID, mode, ref, err, gerr)
	}
	return c.retryJavaStop(ctx, run, mode, base, tries, err)
}

// retryJavaStop 暂时性失败（取消接口或找回任务号的重放，或 Poller 场景缺令牌）：保持当前状态，登记待重试（带本次尝试时间，
// Poller 按退避表等待）；满 javaStopMaxTries 次按停止跟踪处理。
func (c *core) retryJavaStop(ctx context.Context, run *model.NodeRun, mode javaStop, base string, tries int, err error) (ended, pending bool) {
	log := logx.WithContext(ctx)
	tries++
	if tries >= c.javaStopMaxTries() {
		reason, suf := "retry_exhausted", sufJavaRetryOut
		switch {
		case errors.Is(err, javatool.ErrNoBearer):
			reason, suf = "no_bearer", sufJavaNoLogin
		case run.Status == model.RunQueued:
			suf = sufJavaNoRef
		}
		return c.degradeJava(ctx, run, mode, base, reason, suf), false
	}
	log.Infof("canvas: run %d（%s）停止 Java 任务 %s 暂时失败（第 %d 次），下轮重试: %v", run.ID, mode, run.BackendRef.String, tries, err)
	bctx, cancel := detached(ctx)
	defer cancel()
	ok, uerr := c.s.Repo.Run.UpdateIfStatus(bctx, run.ID, run.Status, map[string]any{
		"error_code": fmt.Sprintf("%s%c:%d:%d", javaCancelPending, byte(mode), tries, c.now().Unix()), "error_reason": base,
	})
	if uerr != nil {
		log.Errorf("canvas: run %d 登记待取消失败: %v", run.ID, uerr)
		return false, false
	}
	return false, ok
}

// finishedJava Java 拒绝取消且任务已终态：以 Java 真实结果收尾。
func (c *core) finishedJava(ctx context.Context, run *model.NodeRun, mode javaStop, base string, t *javatool.Task) bool {
	to, code := mode.target()
	if t.Succeeded() && mode != stopAdmin {
		// 取消时 Java 已成功（钱已由 Java 扣）：按「已完成」交付，与 gateway 后端「取消时已成功则交付」口径一致
		logx.WithContext(ctx).Infof("canvas: run %d（%s）取消时 Java 任务 %s 已成功，按成功交付", run.ID, mode, t.TaskUID)
		if _, _, ok := pendingJavaStop(run); ok { // 成功的 run 不留待取消标记
			_, _ = c.s.Repo.Run.UpdateIfStatus(ctx, run.ID, model.RunRunning, map[string]any{"error_code": nil, "error_reason": nil})
		}
		c.apply(ctx, run, ClassifyJava(t))
		return run.Status != model.RunRunning
	}
	suf := sufJavaSucceeded
	if !t.Succeeded() {
		suf = fmt.Sprintf(sufJavaEndedFmt, javaStatusText(t.Status))
	}
	return c.endRun(ctx, run, model.RunRunning, to, code, base+suf, false, false)
}

func javaStatusText(s string) string {
	switch s {
	case javatool.StatusFailed:
		return "失败"
	case javatool.StatusTimeout:
		return "超时"
	case javatool.StatusCancelled:
		return "已取消"
	case javatool.StatusSucceed:
		return "成功"
	}
	return s
}

// degradeJava 无法真实取消：退化为停止跟踪，WARN（logx 无 warn 级别，按 error 级别输出、消息以 WARN 开头、带 check 键）+ 计数，便于运营到 Java 后台手动取消。
func (c *core) degradeJava(ctx context.Context, run *model.NodeRun, mode javaStop, base, reason, suf string) bool {
	to, code := mode.target()
	javaCancelDegraded.Inc(mode.String(), reason)
	logx.WithContext(ctx).Errorw(fmt.Sprintf("WARN canvas: run %d Java 任务未能取消，退化为停止跟踪，请到 Java 后台手动取消 (check=%s)",
		run.ID, javaDegradedCheck),
		logx.Field("run_id", run.ID), logx.Field("user_id", run.UserID), logx.Field("java_task", run.BackendRef.String),
		logx.Field("scene", mode.String()), logx.Field("reason", reason), logx.Field("idempotency_key", javaIdem(run)))
	return c.endRun(ctx, run, run.Status, to, code, base+suf, false, false)
}

// javaIdem run 发给 Java 的幂等键（找不回任务号时运营按它在 Java 查 submit_idempotency_key）。
func javaIdem(run *model.NodeRun) string {
	if jr, ok := parseJavaRequest(run); ok {
		return jr.Idem
	}
	k, _ := runKey(run)
	return k
}

// cancelJavaOrphan 提交期间 run 已被取消 / 强制失败 / 超时：刚拿到的 Java 任务成了孤儿，尽力取消（不改 run）。
func (c *core) cancelJavaOrphan(ctx context.Context, run *model.NodeRun, ref, bearer string) {
	if ref == "" {
		return
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	out, err := c.s.Java.Cancel(bctx, bearer, ref)
	switch out {
	case javatool.CancelCancelled, javatool.CancelAlreadyFinished, javatool.CancelNotFound:
		logx.WithContext(ctx).Infof("canvas: run %d 提交期间已结束，取消孤儿 Java 任务 %s：%s %v", run.ID, ref, out, err)
	default:
		reason := "transient"
		if out == javatool.CancelUnauthorized || errors.Is(err, javatool.ErrNoBearer) {
			reason = "unauthorized"
		}
		javaCancelDegraded.Inc("orphan", reason)
		logx.WithContext(ctx).Errorw(fmt.Sprintf("WARN canvas: run %d 提交期间已结束，孤儿 Java 任务 %s 未能取消，可能仍会执行并扣费，请到 Java 后台手动取消 (check=%s)",
			run.ID, ref, javaDegradedCheck), logx.Field("reason", reason), logx.Field("err", errString(err)))
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
