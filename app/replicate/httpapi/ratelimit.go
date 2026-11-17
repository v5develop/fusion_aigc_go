package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// 限频（P0-4）：每用户 分析 / 方案 / 确认 的每分钟与每日上限（pkg/userquota，固定窗口，北京时间自然日），
// 外加确认时的全局在途 job 上限（库里实时数）。参数取 afv_system_config 的 replicate.rate.*（60 秒生效，0 = 不限）。
// 计数在会话 Redis（多实例共享）；没配 Redis 时退回进程内计数并 WARN（多实例各算各的，仅开发）。
// 命中返回 HTTP 429 + code=4002 reason=RATE_LIMITED + 可读文案 + data.retryAfter（秒），同时带 Retry-After 头。
// 被拒的请求不占额度；请求失败（缺项 / 业务错误）退回本次额度；分析命中单飞复用（同一条视频）不计次。

// ReasonRateLimited 限频命中的 reason。
const ReasonRateLimited = "RATE_LIMITED"

// 限频动作。
const (
	ActionAnalysis = "analysis"
	ActionPlan     = "plan"
	ActionConfirm  = "confirm"
	// ActionAnnotate 改标注 / 再做一条：独立计数（replicate.rate.annotate_*），不占「方案」额度
	ActionAnnotate = "annotate"
	// ActionFeedback 成片反馈：轻量固定限频（不读配置，只为防刷）
	ActionFeedback = "feedback"
)

// 成片反馈的固定限额：每分钟 / 每日（北京时间自然日）。覆盖更新也占次数，正常用户远用不到。
const (
	feedbackPerMinute = 20
	feedbackPerDay    = 300
)

// RateLimitError 限频命中（writeErr 写成 HTTP 429）。
type RateLimitError struct {
	Msg        string
	RetryAfter int // 秒
	Check      string
}

func (e *RateLimitError) Error() string { return "replicate: 限频 判据=" + e.Check + " " + e.Msg }

var cst = time.FixedZone("CST", 8*3600)

// secondsToNextMinute / secondsToMidnight 距下一个窗口的秒数（至少 1）。
func secondsToNextMinute(now time.Time) int {
	return max(1, 60-now.In(cst).Second())
}

func secondsToMidnight(now time.Time) int {
	t := now.In(cst)
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, cst)
	return max(1, int(next.Sub(t).Seconds()))
}

var dayText = map[string]string{
	ActionAnalysis: "今天的免费分析次数用完了，明天 0 点恢复。你仍可使用已分析过的视频",
	ActionPlan:     "今天的方案次数用完了，明天 0 点恢复。已有方案仍然可以确认生成",
	ActionConfirm:  "今天提交生成的次数用完了，明天 0 点恢复",
	ActionAnnotate: "今天修改方案的次数用完了，明天 0 点恢复。已有方案仍然可以确认生成",
	ActionFeedback: "今天提交反馈的次数用完了，明天 0 点恢复，谢谢你的反馈",
}

const (
	minuteText   = "操作太快了，稍等一下再试"
	inFlightText = "现在做同款的人比较多，请稍等几分钟再提交。没有扣点数"
)

// rateLimiter 按动作持有 userquota.Limiter（参数变了重建；计数在 Redis 里，重建不丢）。
type rateLimiter struct {
	rds      redis.UniversalClient
	inflight func(ctx context.Context) (int64, error)
	now      func() time.Time

	mu     sync.Mutex
	lims   map[string]*userquota.Limiter
	sig    map[string]string
	warned bool
}

func newRateLimiter(rds redis.UniversalClient, inflight func(ctx context.Context) (int64, error)) *rateLimiter {
	return &rateLimiter{rds: rds, inflight: inflight, now: time.Now, lims: map[string]*userquota.Limiter{}, sig: map[string]string{}}
}

func perOf(p RateParams, action string) (int, int) {
	switch action {
	case ActionAnalysis:
		return p.AnalysisPerMinute, p.AnalysisPerDay
	case ActionPlan:
		return p.PlanPerMinute, p.PlanPerDay
	case ActionConfirm:
		return p.ConfirmPerMinute, p.ConfirmPerDay
	case ActionAnnotate:
		return p.AnnotatePerMinute, p.AnnotatePerDay
	case ActionFeedback:
		return feedbackPerMinute, feedbackPerDay
	}
	return 0, 0
}

func (l *rateLimiter) limiter(action string, p RateParams) *userquota.Limiter {
	pm, pd := perOf(p, action)
	sig := strconv.Itoa(pm) + "/" + strconv.Itoa(pd)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rds == nil && !l.warned {
		l.warned = true
		logx.Infof("replicate: WARN 没配会话 Redis，限频退回进程内计数（多实例各算各的，仅开发） 判据=RATE_NO_REDIS")
	}
	if lim := l.lims[action]; lim != nil && l.sig[action] == sig {
		return lim
	}
	lim := userquota.New("replicate:"+action, pm, pd, l.rds)
	l.lims[action], l.sig[action] = lim, sig
	return lim
}

// take 申请一次额度；返回的 refund 在请求失败 / 不该计次时调用（退回本次额度）。
func (l *rateLimiter) take(ctx context.Context, action string, uid int64, p RateParams) (refund func(), err error) {
	noop := func() {}
	if l == nil {
		return noop, nil
	}
	if action == ActionConfirm && p.MaxInFlightJobs > 0 && l.inflight != nil {
		n, err := l.inflight(ctx)
		switch {
		case err != nil:
			logx.WithContext(ctx).Errorf("replicate: 读全局在途数失败（放行） 判据=RATE_INFLIGHT_UNKNOWN: %v", err)
		case n >= int64(p.MaxInFlightJobs):
			logx.WithContext(ctx).Infof("replicate: 限频 判据=RATE_GLOBAL_INFLIGHT user=%d 在途=%d 上限=%d", uid, n, p.MaxInFlightJobs)
			return noop, &RateLimitError{Msg: inFlightText, RetryAfter: 60, Check: "RATE_GLOBAL_INFLIGHT"}
		}
	}
	lim := l.limiter(action, p)
	switch lim.Take(ctx, uid) {
	case userquota.MinuteExceed:
		logx.WithContext(ctx).Infof("replicate: 限频 判据=RATE_MINUTE action=%s user=%d", action, uid)
		return noop, &RateLimitError{Msg: minuteText, RetryAfter: secondsToNextMinute(l.now()), Check: "RATE_MINUTE"}
	case userquota.DayExceed:
		logx.WithContext(ctx).Infof("replicate: 限频 判据=RATE_DAY action=%s user=%d", action, uid)
		return noop, &RateLimitError{Msg: dayText[action], RetryAfter: secondsToMidnight(l.now()), Check: "RATE_DAY"}
	}
	return func() { lim.Refund(context.WithoutCancel(ctx), uid) }, nil
}

// writeRateLimited HTTP 429 + 统一信封。
func writeRateLimited(w http.ResponseWriter, r *http.Request, e *RateLimitError) {
	w.Header().Set("Retry-After", fmt.Sprint(e.RetryAfter))
	httpx.WriteJsonCtx(r.Context(), w, http.StatusTooManyRequests, result.Body{Code: xerr.Conflict, Reason: ReasonRateLimited,
		Msg: e.Msg, Data: map[string]any{"retryAfter": e.RetryAfter}})
}
