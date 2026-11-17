package httpapi

import (
	"context"
	"testing"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/xerr"
)

// 成片反馈（CONTRACT §Z）：运行时总开关关着时写入口回 7003，但反馈（读入口口径）照常收；限频是固定的轻量额度，与可配动作互不影响。
func TestFeedbackNotBlockedBySwitch(t *testing.T) {
	rt := newRuntime(core.DefaultConfig(), false, func(context.Context) (map[string]string, error) { return map[string]string{KeyEnabled: "false"}, nil },
		func(cfg core.Config, lf bool) *core.Service { return core.New(cfg, nil, nil) })
	o := (&Module{rt: rt}).Ops()
	ctx := context.Background()
	if _, err := o.Enter(ctx, 7, true); err == nil {
		t.Fatal("总开关关着，写入口应回 7003")
	} else if code, _, _ := xerr.Parse(err); code != xerr.NotReady {
		t.Fatalf("%v", err)
	}
	sc, err := o.Enter(ctx, 7, false)
	if err != nil {
		t.Fatalf("反馈走读入口，总开关关着也要能进：%v", err)
	}
	// 非法参数在内核之前被拦（不会碰库）
	if _, err := sc.SubmitFeedback(ctx, core.FeedbackReq{JobID: 0, Rating: "up"}); err == nil {
		t.Fatal("job_id 不合法应报错")
	}
}

func TestFeedbackRateLimitIsLightAndFixed(t *testing.T) {
	l := newRateLimiter(nil, nil)
	ctx := context.Background()
	// 配置里的动作额度全为 0（不限）时，反馈仍有固定的每分钟上限
	p := RateParams{}
	for i := 0; i < feedbackPerMinute; i++ {
		if _, err := l.take(ctx, ActionFeedback, 9, p); err != nil {
			t.Fatalf("第 %d 次应放行：%v", i+1, err)
		}
	}
	_, err := l.take(ctx, ActionFeedback, 9, p)
	if rl, ok := err.(*RateLimitError); !ok || rl.Check != "RATE_MINUTE" {
		t.Fatalf("超过每分钟上限应限频：%v", err)
	}
	// 别的用户不受影响；失败退回的额度可再用
	refund, err := l.take(ctx, ActionFeedback, 10, p)
	if err != nil {
		t.Fatal(err)
	}
	refund()
}
