package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"
)

func TestApplyParamsModerationAndRate(t *testing.T) {
	p := applyParams(core.DefaultConfig(), true, map[string]string{
		KeyModerationEnabled: "false", KeyModerationFailOpen: "true", KeyModerationReviewBlock: "false",
		KeyModerationImageGroup: "moderation.image.x", KeyRateAnalysisPerDay: "3", KeyRatePlanPerMinute: "0",
		KeyRateMaxInFlightJobs: "abc",
	})
	if !p.Cfg.ModerationOff || !p.Cfg.ModerationFailOpen || !p.Cfg.ModerationReviewPass || p.Cfg.ModerationImageGroup != "moderation.image.x" {
		t.Fatalf("审核参数：%+v", p.Cfg)
	}
	if p.Rate.AnalysisPerDay != 3 || p.Rate.PlanPerMinute != 0 || p.Rate.MaxInFlightJobs != DefaultRate().MaxInFlightJobs {
		t.Fatalf("限频参数（非法值用默认）：%+v", p.Rate)
	}
	d := applyParams(core.DefaultConfig(), true, nil)
	if d.Cfg.ModerationOff || d.Cfg.ModerationFailOpen || d.Cfg.ModerationReviewPass || d.Rate != DefaultRate() {
		t.Fatalf("默认：审核开、fail-close、review 按违规：%+v", d)
	}
	if signature(p) == signature(d) {
		t.Fatal("审核参数变了要重建内核")
	}
}

func TestRateLimiterMinuteDayRefundAndInflight(t *testing.T) {
	inflight := int64(0)
	var inflightErr error
	l := newRateLimiter(nil, func(context.Context) (int64, error) { return inflight, inflightErr })
	now := time.Date(2026, 11, 14, 23, 59, 30, 0, cst)
	l.now = func() time.Time { return now }
	p := RateParams{AnalysisPerMinute: 2, AnalysisPerDay: 3, ConfirmPerMinute: 10, ConfirmPerDay: 10, MaxInFlightJobs: 5}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := l.take(ctx, ActionAnalysis, 1, p); err != nil {
			t.Fatal(err)
		}
	}
	_, err := l.take(ctx, ActionAnalysis, 1, p)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Check != "RATE_MINUTE" || rl.RetryAfter != 30 {
		t.Fatalf("分钟超限，30 秒后恢复：%+v", err)
	}
	if _, err := l.take(ctx, ActionAnalysis, 2, p); err != nil {
		t.Fatal("别的用户不受影响")
	}
	// 退回：失败的请求不占额度
	refund, err := l.take(ctx, ActionConfirm, 1, p)
	if err != nil {
		t.Fatal(err)
	}
	refund()
	// 全局在途
	inflight = 5
	if _, err := l.take(ctx, ActionConfirm, 1, p); !errors.As(err, &rl) || rl.Check != "RATE_GLOBAL_INFLIGHT" || !strings.Contains(rl.Msg, "没有扣点数") {
		t.Fatalf("全局在途满：%v", err)
	}
	inflightErr = errors.New("db down")
	if _, err := l.take(ctx, ActionConfirm, 1, p); err != nil {
		t.Fatalf("读在途失败放行：%v", err)
	}
	// 日上限（分钟不限；计数窗口用真实时钟，retryAfter 按 l.now 算到北京时间 0 点）
	inflightErr, inflight = nil, 0
	l.now = func() time.Time { return time.Date(2026, 11, 14, 23, 58, 0, 0, cst) }
	pd := RateParams{AnalysisPerDay: 1}
	if _, err := l.take(ctx, ActionAnalysis, 9, pd); err != nil {
		t.Fatal(err)
	}
	if _, err := l.take(ctx, ActionAnalysis, 9, pd); !errors.As(err, &rl) || rl.Check != "RATE_DAY" || rl.RetryAfter != 120 || !strings.Contains(rl.Msg, "明天 0 点") {
		t.Fatalf("日超限，到 0 点：%+v", err)
	}
	// 参数改成不限：放行
	if _, err := l.take(ctx, ActionAnalysis, 1, RateParams{}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRateLimited429(t *testing.T) {
	result.Setup()
	w := httptest.NewRecorder()
	writeErr(w, httptest.NewRequest("POST", "/x", nil), &RateLimitError{Msg: minuteText, RetryAfter: 12, Check: "RATE_MINUTE"})
	if w.Code != 429 || w.Header().Get("Retry-After") != "12" {
		t.Fatalf("HTTP 429 + Retry-After：%d %v", w.Code, w.Header())
	}
	var body struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
		Msg    string `json:"msg"`
		Data   struct {
			RetryAfter int `json:"retryAfter"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != xerr.Conflict || body.Reason != ReasonRateLimited || body.Msg != minuteText || body.Data.RetryAfter != 12 {
		t.Fatalf("信封：%s", w.Body.String())
	}
}
