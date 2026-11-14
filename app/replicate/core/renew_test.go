package core

import (
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
)

// 冻结成功写镜像时记 held_at，之后镜像怎么改写都不动它。
func TestHeldAtWrittenOnceAtHold(t *testing.T) {
	e := newEnv(t, false)
	t0 := e.clock
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	c := e.charges(jv.JobID)[0]
	if c.Status != model.ChargeHeld || c.HeldAt == nil || !c.HeldAt.Equal(t0) {
		t.Fatalf("冻结成功应记 held_at=%v：%+v", t0, c)
	}
	e.ledger.confirmErr = 1
	e.clock = e.clock.Add(10 * time.Minute)
	e.run(jv.JobID, 3)
	c = e.charges(jv.JobID)[0]
	if c.HeldAt == nil || !c.HeldAt.Equal(t0) {
		t.Fatalf("held_at 写一次后不再改：%+v", c.HeldAt)
	}
}

// 扣费待重试（6 + confirm:）的单：续期按 held_at 起算（镜像 updated_at 每轮重试都被改写，不能用它），
// 到 RenewAfter 之前不续（续了会缩短到期时间），到了之后每轮续。
func TestRenewConfirmPendingFromHeldAt(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	req := e.rFirst(jv.JobID, 1)
	e.ledger.confirmErr = 1 << 30 // 确认扣除一直失败
	e.run(jv.JobID, 3)
	if c := e.charges(jv.JobID)[0]; !confirmPending(c) {
		t.Fatalf("应进入扣费待重试：%+v", c)
	}
	e.clock = e.clock.Add(185 * time.Minute) // 距 held_at 185 分钟 < RenewAfter(186)
	e.run(jv.JobID, 1)
	if e.ledger.renews[req] != 0 {
		t.Fatalf("没到 RenewAfter 不得续期：%v", e.ledger.renews)
	}
	e.clock = e.clock.Add(2 * time.Minute)
	e.run(jv.JobID, 1)
	if e.ledger.renews[req] == 0 {
		t.Fatalf("扣费待重试的单超过 RenewAfter 应续期：%v", e.ledger.renews)
	}
	// 续期期间单一直是 HELD；恢复后照常扣除交付
	e.ledger.confirmErr = 0
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" {
		t.Fatalf("恢复后应扣除并交付：%s", jv.Status)
	}
	e.assertMoney(jv.JobID)
}

// 终态 job 上的扣费待重试也续期；旧行没有 held_at 的扣费待重试推不出冻结时刻，不续。
func TestRenewConfirmPendingOnTerminalJob(t *testing.T) {
	e := newEnv(t, false)
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID, 3)
	if jv.Status != "succeeded" {
		t.Fatalf("%s", jv.Status)
	}
	req := e.rFirst(jv.JobID, 1)
	// 构造：终态 job、段已成功、镜像停在扣费待重试、真实预留单仍 HELD（确认扣除调用失败后崩溃）
	e.ledger.res[req].status = "HELD"
	held := e.clock.Add(-190 * time.Minute)
	e.store.mu.Lock()
	for _, c := range e.store.charges {
		if c.RequestID == req {
			c.Status, c.Reason, c.HeldAt = model.ChargeSettleFailed, ptr(model.SettleReasonConfirm+"timeout"), nil
		}
	}
	e.store.mu.Unlock()
	e.ledger.confirmErr = 1 << 30
	e.run(jv.JobID, 1)
	if e.ledger.renews[req] != 0 {
		t.Fatalf("没有 held_at 的扣费待重试不续：%v", e.ledger.renews)
	}
	e.store.mu.Lock()
	for _, c := range e.store.charges {
		if c.RequestID == req {
			c.HeldAt = &held
		}
	}
	e.store.mu.Unlock()
	e.run(jv.JobID, 1)
	if e.ledger.renews[req] == 0 {
		t.Fatalf("终态 job 上的扣费待重试应续期：%v", e.ledger.renews)
	}
	e.ledger.confirmErr = 0
	e.run(jv.JobID, 2)
	e.assertMoney(jv.JobID)
}
