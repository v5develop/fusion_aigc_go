package core

import (
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
)

func asyncModEnv(t *testing.T, mut func(*Config)) (*env, *memOps) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, func(c *Config) {
		c.ModerationPlanWait = 0 // 生产默认：方案不等审核
		c.ModerationConfirmWait = 50 * time.Millisecond
		if mut != nil {
			mut(c)
		}
	}, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	return e, ops
}

// 方案生成立即返回（审核状态 pending），审核在后台进行；确认时结论还没出 → MODERATION_PENDING，不建 job、不预留。
// 结论出来后再确认直接用已有结论（不再调网关）。
func TestModerationAsyncConfirmBeforeVerdict(t *testing.T) {
	e, ops := asyncModEnv(t, nil)
	e.gw.analysis[opModerationImage] = verdicts("pass")
	gate := make(chan struct{})
	e.gw.modGate = gate
	pv := e.plan(e.seedAnalysis(10))
	if pv.Moderation == nil || pv.Moderation.Status != "pending" {
		t.Fatalf("方案应立即返回、审核 pending：%+v", pv.Moderation)
	}
	start := time.Now()
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonModerationPending || !strings.Contains(err.Error(), "几秒后再点确认") {
		t.Fatalf("结论未出应回 MODERATION_PENDING：%v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("确认时应先等后台审核（ModerationConfirmWait）")
	}
	if e.ledger.holds != 0 {
		t.Fatalf("结论未出不得预留点数：holds=%d", e.ledger.holds)
	}
	if j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1"); j != nil {
		t.Fatal("结论未出不得建 job")
	}
	close(gate)
	e.svc.waitModerationIdle()
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if m, _ := ops.GetModeration(e.ctx, p.ID, p.PlanHash); m == nil || m.Status != model.ModerationPassed {
		t.Fatalf("后台审核应落结论：%+v", m)
	}
	if v, _ := e.svc.GetPlan(e.ctx, uid, pv.PlanNo); v.Moderation.Status != "passed" {
		t.Fatalf("GET 方案应看到结论：%+v", v.Moderation)
	}
	calls := modCalls(e)
	jv := e.confirm(pv, "", "k2")
	if jv.Money.Held != 100 || modCalls(e) != calls {
		t.Fatalf("结论已出应直接用（不再调网关）并正常预留：%+v calls=%d→%d", jv.Money, calls, modCalls(e))
	}
}

// 确认时结论已出（后台预审在确认前完成）：直接用，不等、不重审；不通过照旧拒绝。
func TestModerationAsyncVerdictReadyAtConfirm(t *testing.T) {
	e, _ := asyncModEnv(t, func(c *Config) { c.ModerationConfirmWait = 5 * time.Second })
	e.gw.analysis[opModerationImage] = verdicts("block")
	pv := e.plan(e.seedAnalysis(10))
	e.svc.waitModerationIdle()
	calls := modCalls(e)
	start := time.Now()
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonModerationRejected || e.ledger.holds != 0 {
		t.Fatalf("已有不通过结论应直接拒绝：%v holds=%d", err, e.ledger.holds)
	}
	if time.Since(start) > time.Second || modCalls(e) != calls {
		t.Fatalf("已有结论不应再等 / 再审：calls=%d→%d", calls, modCalls(e))
	}
}

// 确认时没有预审（如别的实例生成的方案）：当场发起，3 秒内出结论就继续确认。
func TestModerationAsyncConfirmWaitsShortVerdict(t *testing.T) {
	e, _ := asyncModEnv(t, func(c *Config) { c.ModerationOff = true })
	e.gw.analysis[opModerationImage] = verdicts("pass")
	pv := e.plan(e.seedAnalysis(10)) // 生成时审核关着：没有预审
	e.svc.cfg.ModerationOff = false
	e.svc.cfg.ModerationConfirmWait = 5 * time.Second
	jv := e.confirm(pv, "", "k1")
	if jv.Money.Held != 100 || modCalls(e) != 1 {
		t.Fatalf("确认时当场审、等到结论后应正常确认：%+v calls=%d", jv.Money, modCalls(e))
	}
}

// fail-close 照旧：后台审核故障 → 确认回 MODERATION_UNAVAILABLE；fail-open → 放行。
func TestModerationAsyncFailCloseAndOpen(t *testing.T) {
	e, _ := asyncModEnv(t, func(c *Config) { c.ModerationConfirmWait = 5 * time.Second })
	pv := e.plan(e.seedAnalysis(10)) // 没配审核结果：替身网关回读不懂的结果 → 故障
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonModerationUnavailable || e.ledger.holds != 0 {
		t.Fatalf("fail-close：%v holds=%d", err, e.ledger.holds)
	}
	e2, _ := asyncModEnv(t, func(c *Config) { c.ModerationConfirmWait = 5 * time.Second; c.ModerationFailOpen = true })
	if jv := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1"); jv.Money.Held != 100 {
		t.Fatalf("fail-open 放行：%+v", jv.Money)
	}
}
