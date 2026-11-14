package core

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/notify"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 站内通知（P0-5，EXPERIENCE §3.7.2 / §8.5）：三处发——第 1 段好了待决定、全部完成（含部分成功说明）、某段最终失败已退点。
//
//   - 由状态推导，不由事件驱动：每次推进（Advance）末尾按 job 当前状态判断该发什么，去重键 replicate:<job>:<类> 保证
//     同 job 同类只 1 次（pkg/notify 唯一键）。崩溃、重放、多实例都不会多发；漏发只会推迟到下一次推进。
//   - 钱的字样以真实结果为准：退点还在处理中（镜像未结）时不发「已退回」，等补扫退成功后的那一轮再发。
//   - 写 Java 通知中心表 afv_notification（经 pkg/notify，定向 USER、SYSTEM 类）；web 现有铃铛直接展示。
//   - 通知失败只打日志，绝不影响推进 / 结算。用户按类关闭本期不做。
// ---------------------------------------------------------------------------------------------

// 通知类型（pkg/notify kind）。
const (
	KindFirstReady = "replicate.first_ready"
	KindDone       = "replicate.done"
	KindSegFailed  = "replicate.seg_failed"
)

// Notifier 站内通知发送（*notify.Service 直接满足）。
type Notifier interface {
	Send(ctx context.Context, m notify.Message) (notify.Outcome, error)
}

// WithNotifier 装配站内通知（不装配则不发）。
func WithNotifier(n Notifier) Option { return func(s *Service) { s.notifier = n } }

// sentCache 本进程已确认发过（或用户已关）的去重键，省掉重复的 INSERT（库里唯一键才是真正的去重）。
type sentCache struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func (c *sentCache) has(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[k]
	return ok
}

func (c *sentCache) add(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 20000 {
		c.m = map[string]struct{}{}
	}
	c.m[k] = struct{}{}
}

// JobLink 用户端任务页（EXPERIENCE：用 ?job= 回到任务）。
func JobLink(jobID int64) string { return "/video/replicate?job=" + strconv.FormatInt(jobID, 10) }

func dedupeKey(jobID int64, kind string) string { return fmt.Sprintf("replicate:%d:%s", jobID, kind) }

func (s *Service) send(ctx context.Context, j *model.Job, kind, title, body string) {
	key := dedupeKey(j.ID, kind)
	if s.sent.has(key) {
		return
	}
	out, err := s.notifier.Send(context.WithoutCancel(ctx), notify.Message{UserID: j.UserID, Kind: kind, Title: title, Body: body,
		Link: JobLink(j.ID), BizType: "replicate_job", BizID: strconv.FormatInt(j.ID, 10), DedupeKey: key})
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 站内通知发送失败（不影响任务） 判据=NOTIFY_FAILED job=%d kind=%s: %v", j.ID, kind, err)
		return
	}
	s.sent.add(key)
	if out == notify.Sent {
		logx.WithContext(ctx).Infof("replicate: 站内通知已发 job=%d kind=%s", j.ID, kind)
	}
}

// notifyJob 按 job 当前状态补发应发的通知（幂等）。任何错误只打日志。
func (s *Service) notifyJob(ctx context.Context, jobID int64) {
	if s.notifier == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logx.WithContext(ctx).Errorf("replicate: 通知判断 panic（不影响任务） job=%d: %v", jobID, r)
		}
	}()
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil || j == nil {
		return
	}
	final := model.JobFinal(j.Status)
	if j.Status != model.JobWaiting && !final && j.SegFailed == 0 {
		return // 在途且还没有失败段：没什么可发
	}
	if j.Status == model.JobWaiting && j.SegTotal > 1 {
		s.send(ctx, j, KindFirstReady, "第 1 段做好了", "视频做同款：第 1 段做好了，来看看再决定要不要继续")
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return
	}
	// 失败段（没做成）：链尾是失败，或被后台强制收尾取消；且这些步骤的预留全部结清（退回成功 / 不计费），才能说「已退回」。
	// 退回点数按计费请求号去重后累加：自动重试（#r1…）共用原预留单，不能按步骤重复算。
	notDone := func(st *model.Step) bool {
		return st.Status == model.StepFailed || (st.Status == model.StepCanceled && deref(st.ErrorCode) == codeAdminForce)
	}
	var failedSegs []int
	var refunded int64
	pending := false
	counted := map[string]bool{}
	for k := 1; k <= j.SegTotal; k++ {
		chain := segChain(steps, k)
		if len(chain) == 0 || !notDone(chain[len(chain)-1]) {
			continue
		}
		segPending := false
		var segRefunded int64
		var segReqs []string
		segSeen := map[string]bool{}
		for _, st := range chain {
			if !notDone(st) || st.BillingRequestID == nil || counted[*st.BillingRequestID] || segSeen[*st.BillingRequestID] {
				continue
			}
			segSeen[*st.BillingRequestID] = true
			if c := chargeOf(charges, st.BillingRequestID); c != nil {
				switch {
				case model.ChargeOpen(c.Status):
					segPending = true
				case c.Status == model.ChargeRefunded:
					segRefunded += c.Points
					segReqs = append(segReqs, c.RequestID)
				}
			}
		}
		if segPending {
			pending = true
			continue
		}
		for _, r := range segReqs {
			counted[r] = true
		}
		failedSegs = append(failedSegs, k)
		refunded += segRefunded
	}
	if len(failedSegs) > 0 {
		money := "没有扣点数"
		if refunded > 0 {
			money = fmt.Sprintf("已退回 %d 点", refunded)
		}
		switch {
		case j.Status == model.JobFailed && !pending:
			if refunded > 0 {
				money = fmt.Sprintf("已全部退回 %d 点", refunded)
			}
			s.send(ctx, j, KindSegFailed, "视频做同款没有生成成功", fmt.Sprintf("这次没有生成成功，%s。可以换个模型再试", money))
		case j.Status != model.JobFailed:
			k := failedSegs[0]
			title := fmt.Sprintf("第 %d 段没生成出来", k)
			body := fmt.Sprintf("视频做同款：第 %d 段这次没生成出来，%s。可以重做这一段", k, money)
			if len(failedSegs) > 1 {
				title = fmt.Sprintf("有 %d 段没生成出来", len(failedSegs))
				body = fmt.Sprintf("视频做同款：第 %s 段这次没生成出来，%s。可以逐段重做", joinInts(failedSegs), money)
			}
			s.send(ctx, j, KindSegFailed, title, body)
		}
	}
	if !final || pending {
		return
	}
	switch j.Status {
	case model.JobSucceeded:
		stop := deref(j.StopReason)
		switch {
		case stop == "user_stop":
			return // 用户自己点的「不继续」：不打扰
		case stop == codeAdminForce:
			// 后台强制收尾（待继续 → 只交付第 1 段）：其余段计入「没做成」，从未预留、没有收费
			s.send(ctx, j, KindDone, "视频做同款已结束", fmt.Sprintf("做好了 %d 段，%d 段没做成，没有收费。点击查看", j.SegDone, max(0, j.SegTotal-j.SegDone)))
		case stop == "continue_expired":
			s.send(ctx, j, KindDone, "视频做同款已结束", "超过 24 小时没有继续，已为你结束，只保留了第 1 段，没有再收费")
		case j.SegTotal > 1:
			s.send(ctx, j, KindDone, "视频做同款已完成", fmt.Sprintf("%d 段全部成功，点击查看", j.SegTotal))
		default:
			s.send(ctx, j, KindDone, "视频做同款已完成", "做好了！点击查看")
		}
	case model.JobPartial:
		failed := len(failedSegs)
		money := "没有扣点数"
		if refunded > 0 {
			money = fmt.Sprintf("已退回 %d 点", refunded)
		}
		s.send(ctx, j, KindDone, "视频做同款部分完成", fmt.Sprintf("做好了 %d 段，%d 段没成功，%s。点击查看", j.SegDone, failed, money))
	}
}
