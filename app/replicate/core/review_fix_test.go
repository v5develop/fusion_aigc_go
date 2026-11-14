package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

// 代码审查确认问题（H1–H3 / M1–M5 / L1–L4 / 待确认项）的回归用例。

// rebuild 换一套配置 / 执行者重建内核（同一个 store / ledger / 网关：模拟改运行参数或进程重启）。
func (e *env) rebuild(owner string, mut func(*Config)) {
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	if mut != nil {
		mut(&cfg)
	}
	opts := []Option{WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }), WithOwner(owner),
		WithURLOwner(testOwner), WithSigner(e.signer)}
	if e.cutter != nil {
		opts = append(opts, WithCutter(e.cutter))
	}
	e.svc = New(cfg, e.store, e.ledger, opts...)
}

func (e *env) step(jobID int64, key string) *model.Step {
	for _, st := range e.steps(jobID) {
		if st.StepKey == key {
			return st
		}
	}
	return nil
}

func (e *env) job(id int64) *model.Job {
	j, _ := e.store.GetJob(e.ctx, id)
	return j
}

// H1：在途段跨进程重启（实例标识变了）仍能失败 → 自动重试，并改写 lease_owner。
func TestH1RetryAfterRestartOwnerChanged(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	// 第一轮由 test-owner 领段并提交；fake 网关在第一次 Get 才给结果，所以仍是 RUNNING
	st := e.step(jv.JobID, "seg/01/generate")
	_, _ = e.store.UpdateStep(e.ctx, st.ID, model.StepPending, map[string]any{"status": model.StepRunning, "lease_owner": "old-pod-1"})
	e.rebuild("new-pod-2", nil) // 重启：实例标识变了
	jv = e.run(jv.JobID, 2)     // 第 1 轮按幂等键提交、第 2 轮拿到失败结果
	if got := e.step(jv.JobID, "seg/01/generate"); got.Status != model.StepFailed || deref(got.LeaseOwner) != "new-pod-2" {
		t.Fatalf("原步骤应被新实例接管并失败：status=%s owner=%s", got.Status, deref(got.LeaseOwner))
	}
	if e.step(jv.JobID, "seg/01/generate#r1") == nil {
		t.Fatal("换了实例也必须建出自动重试步骤（旧实现带 lease_owner 条件会永远停在 RUNNING）")
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

// H2：DueJobs 按 next_run_at 轮转，limit 很小也不会饿死后面的 job；待继续不到截止时间不选。
func TestH2TickRotatesNoStarvation(t *testing.T) {
	e := newEnv(t, false)
	var ids []int64
	for i := 0; i < 5; i++ {
		jv := e.confirm(e.plan(e.seedAnalysis(10+float64(i))), "", "k"+string(rune('a'+i)))
		ids = append(ids, jv.JobID)
	}
	for i := 0; i < 3; i++ { // 每轮 2 条，3 轮覆盖 5 条（时钟不动：推过的 next_run_at 在未来，排不到前面）
		e.svc.Tick(e.ctx, 2)
	}
	for _, id := range ids {
		if e.gw.submitted(idemKey(id, "seg/01/generate")) != 1 {
			t.Fatalf("job %d 应被推进到（不被前面的 job 饿死）：%v", id, e.gw.submits)
		}
	}
	if j := e.job(ids[0]); j.NextRunAt == nil || !j.NextRunAt.After(e.clock) {
		t.Fatalf("推进后应写回 next_run_at：%+v", j.NextRunAt)
	}
	// 待继续：截止时间之前不选
	w := newEnv(t, true)
	wj := w.run(w.confirm(w.plan(w.seedAnalysis(32, 12, 25)), "first", "k1").JobID, 3)
	if wj.Status != "waiting" {
		t.Fatalf("%+v", wj)
	}
	if due, _ := w.store.DueJobs(w.ctx, 10); len(due) != 0 {
		t.Fatalf("待继续未到期不应被取：%v", due)
	}
	w.clock = w.clock.Add(25 * time.Hour)
	if due, _ := w.store.DueJobs(w.ctx, 10); len(due) != 1 {
		t.Fatalf("到期应被取：%v", due)
	}
}

// H2：分析同理按 next_run_at 轮转。
func TestH2AnalysesRotate(t *testing.T) {
	e := newEnv(t, false)
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 10, HasAudio: false}
	var nos []string
	for i := 0; i < 3; i++ {
		v, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("s"+string(rune('a'+i)))})
		if err != nil {
			t.Fatal(err)
		}
		nos = append(nos, v.AnalysisNo)
	}
	e.svc.Tick(e.ctx, 1)
	e.svc.Tick(e.ctx, 1)
	e.svc.Tick(e.ctx, 1)
	for _, no := range nos {
		a, _ := e.store.GetAnalysisByNo(e.ctx, uid, no)
		if a.Stage == model.StageIngest {
			t.Fatalf("分析 %s 没被推进（饿死）", no)
		}
	}
}

// H3：终态后退点失败，补扫带退避重试直到退回成功。
func TestH3RefundRetriedAfterFinalWithBackoff(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	e.ledger.refundErrs = 3
	for i := 0; i < 10 && !model.JobFinal(e.job(jv.JobID).Status); i++ {
		e.clock = e.clock.Add(5 * time.Second) // 在途 job 每轮推进后 3 秒再推
		e.svc.Tick(e.ctx, 10)
	}
	j := e.job(jv.JobID)
	c := e.charges(jv.JobID)[0]
	if j.Status != model.JobFailed || c.Status != model.ChargeSettleFailed || !strings.HasPrefix(deref(c.Reason), model.SettleReasonRefund) {
		t.Fatalf("应终态 + 退点待重试：job=%d charge=%d %s", j.Status, c.Status, deref(c.Reason))
	}
	if v, _ := e.svc.JobView(e.ctx, uid, jv.JobID); v.Money.RefundPending != 100 || !strings.Contains(v.Money.Text, "退点正在处理中") {
		t.Fatalf("%+v", v.Money)
	}
	// 退避：刚推过，下一轮不取
	if due, _ := e.store.DueJobs(e.ctx, 10); len(due) != 0 {
		t.Fatalf("退避期内不应再取：%v", due)
	}
	for i := 0; i < 5 && e.charges(jv.JobID)[0].Status != model.ChargeRefunded; i++ {
		e.clock = e.clock.Add(time.Minute)
		e.svc.Tick(e.ctx, 10)
	}
	if e.charges(jv.JobID)[0].Status != model.ChargeRefunded || e.ledger.status(e.rFirst(jv.JobID, 1)) != "REFUNDED" {
		t.Fatalf("补扫应退回：%+v", e.charges(jv.JobID)[0])
	}
	if due, _ := e.store.DueJobs(e.ctx, 10); len(due) != 0 {
		t.Fatalf("结清后不再取：%v", due)
	}
}

// H3：确认接口客户端在逐段预留中途断开（ctx 取消）：已冻结的段照样退回（脱离请求上下文）。
func TestH3ConfirmClientDisconnectRefundsHeld(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	ctx, cancel := context.WithCancel(e.ctx)
	n := 0
	e.ledger.onHold = func(string) {
		if n++; n == 2 {
			cancel() // 冻结第 2 段时客户端断开
		}
	}
	_, err := e.svc.Confirm(ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, Stage: "all", IdempotencyKey: "k1"})
	if err == nil {
		t.Fatal("断开应失败")
	}
	j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1")
	if e.ledger.sum("HELD") != 0 || e.ledger.status(e.rFirst(j.ID, 1)) != "REFUNDED" {
		t.Fatalf("已冻结的第 1 段必须退回：%s", e.ledger.status(e.rFirst(j.ID, 1)))
	}
	if j.Status != model.JobFailed {
		t.Fatalf("job 应失败：%d", j.Status)
	}
	for _, st := range e.steps(j.ID) {
		if st.Status == model.StepPending {
			t.Fatalf("提交失败的 job 不应留 PENDING 步骤：%+v", st)
		}
	}
	if p, _ := e.store.GetPlan(e.ctx, j.PlanID); p.Status != model.PlanReady {
		t.Fatal("方案应回到可确认")
	}
}

// H3：回收卡住的提交时退点失败 → 终态 job 带未结镜像，补扫补退。
func TestH3ReapSubmittingRefundFailureSwept(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	in, _ := e.svc.jobInput(e.ctx, p)
	j, _, _ := e.store.CreateJob(e.ctx, &model.Job{UserID: uid, RequestID: "crash", PlanID: p.ID, PlanHash: p.PlanHash, Status: model.JobSubmitting, SegTotal: 1, Input: []byte(`{}`)},
		func(id int64) (string, []*model.Step, []*model.Charge) {
			return "t", []*model.Step{{JobID: id, StepKey: genKey(1, ""), Kind: model.StepKindGateway, SegIndex: 1, Status: model.StepPending,
					IdemKey: idemKey(id, genKey(1, "")), BillingRequestID: ptr(reqFirst("", id, 1))}},
				[]*model.Charge{{JobID: id, UserID: uid, SegIndex: 1, Stage: "all", RequestID: reqFirst("", id, 1), Seconds: in.Segments[0].GenDuration}}
		})
	_, _ = e.ledger.Hold(e.ctx, e.svc.charge(uid, e.rFirst(j.ID, 1), in.ModelGroup, in.Resolution, in.Segments[0].GenDuration))
	e.ledger.refundErrs = 1
	e.clock = e.clock.Add(6 * time.Minute)
	e.svc.Tick(e.ctx, 10)
	if cur := e.job(j.ID); cur.Status != model.JobFailed || e.charges(j.ID)[0].Status != model.ChargeSettleFailed {
		t.Fatalf("回收后退点失败应留结算待重试：job=%d charge=%d", cur.Status, e.charges(j.ID)[0].Status)
	}
	if !strings.Contains(deref(e.job(j.ID).ErrorReason), "会自动退回") {
		t.Fatalf("退点没成功时不能说没有扣点数：%s", deref(e.job(j.ID).ErrorReason))
	}
	e.clock = e.clock.Add(time.Minute)
	e.svc.Tick(e.ctx, 10)
	if e.ledger.sum("HELD") != 0 || e.charges(j.ID)[0].Status != model.ChargeRefunded {
		t.Fatalf("补扫应退回：%+v", e.charges(j.ID)[0])
	}
}

// M1：重做预留失败（点数不足）不阻塞后续重做；视图 / 汇总跳过它；编号不复用。
func TestM1RedoHoldFailureDoesNotBlockNextRedo(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "failed" {
		t.Fatalf("%+v", jv)
	}
	e.ledger.balance = 0
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"}); err == nil || !strings.Contains(err.Error(), "点数不足") {
		t.Fatalf("应点数不足：%v", err)
	}
	if st := e.step(jv.JobID, "seg/01/generate#u1"); st == nil || deref(st.ErrorCode) != codeHoldFailed {
		t.Fatalf("预留失败的重做步骤应是 HOLD_FAILED：%+v", st)
	}
	if e.job(jv.JobID).RedoCount != 0 {
		t.Fatal("预留没成功应还回重做名额")
	}
	v, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
	if v.Segments[0].State != "failed" || !v.Segments[0].Redoable || v.Segments[0].Redos != 0 {
		t.Fatalf("视图应跳过预留失败的步骤，仍显示原失败且可重做：%+v", v.Segments[0])
	}
	e.ledger.balance = 10000
	jv, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r2"})
	if err != nil || jv.Status != "running" {
		t.Fatalf("充值后重做应成功：%v", err)
	}
	if e.step(jv.JobID, "seg/01/generate#u2") == nil || e.ledger.status(e.rRedo(jv.JobID, 1, 2)) != "HELD" {
		t.Fatal("新重做必须用新编号 #u2 / -u2（不复用 #u1）")
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Segments[0].Redos != 1 {
		t.Fatalf("%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

// M1：合成时缺段（重做预留失败后仍没成功）用失败占位，不静默省略。
func TestM1FinalizePlaceholderAfterVoidRedo(t *testing.T) {
	e := newEnv(t, true)
	fc := withComposer(e, "font-rid")
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
	e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "partial" {
		t.Fatalf("%+v", jv)
	}
	e.ledger.balance = 0
	_, _ = e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1"})
	jv = e.run(jv.JobID, 2)
	d := resultDoc(t, e, jv.JobID)
	if d.Kind != ResultPreview || len(d.Missing) != 1 || d.Missing[0] != 2 {
		t.Fatalf("第 2 段应占位：%+v", d)
	}
	if len(fc.concats) == 0 || fc.concats[len(fc.concats)-1][1].Placeholder == nil {
		t.Fatalf("拼接应含占位：%+v", fc.concats)
	}
}

// M2：方案过期后同输入重新生成：刷新有效期与报价，能再确认；过期期间视图派生 EXPIRED。
func TestM2RegenerateExpiredPlan(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(10)
	pv := e.plan(a)
	e.clock = e.clock.Add(8 * 24 * time.Hour)
	if q, _ := e.svc.QuotePlan(e.ctx, uid, pv.PlanNo); q.Status != PlanViewExpired {
		t.Fatalf("过期方案视图应是 EXPIRED：%s", q.Status)
	}
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"}); reasonOf(err) != "PLAN_EXPIRED" {
		t.Fatalf("%v", err)
	}
	pv2 := e.plan(a) // 重新生成方案（同输入）
	if pv2.PlanNo != pv.PlanNo || pv2.Status != model.PlanReady || !pv2.ExpiresAt.After(e.clock) {
		t.Fatalf("应刷新同一张方案：%+v", pv2)
	}
	if jv := e.confirm(pv2, "", "k2"); jv.Status != "queued" {
		t.Fatalf("%+v", jv)
	}
}

// M3：长任务预留续期；排队期间预留被回收的段领段前识别、不出片。
func TestM3RenewAndPrecheckBeforeStart(t *testing.T) {
	e := newEnv(t, true)
	e.rebuild("test-owner", func(c *Config) { c.MaxInFlight = 1 })
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
	_ = e.svc.Advance(e.ctx, jv.JobID)       // 第 1 段在途，2/3 排队
	e.clock = e.clock.Add(180 * time.Minute) // 还没到 HoldTTL−租约：不续（续了会把到期缩短）
	_ = e.svc.Advance(e.ctx, jv.JobID)
	if len(e.ledger.renews) != 0 {
		t.Fatalf("到期前 15 分钟之前不得续期：%v", e.ledger.renews)
	}
	e.clock = e.clock.Add(7 * time.Minute)
	_ = e.svc.Advance(e.ctx, jv.JobID)
	if e.ledger.renews[e.rFirst(jv.JobID, 2)] == 0 || e.ledger.renews[e.rFirst(jv.JobID, 3)] == 0 {
		t.Fatalf("冻结超过 RenewAfter 的在途 / 排队段应续期：%v", e.ledger.renews)
	}
	e.ledger.expire(e.rFirst(jv.JobID, 3)) // 第 3 段排队时被回收
	jv = e.run(jv.JobID, 6)
	if e.gw.submitted("seg/03/generate") != 0 {
		t.Fatal("预留已失效的段不得出片")
	}
	if jv.Segments[2].State != "failed" || !strings.Contains(jv.Segments[2].Text, "预留已失效") || jv.Status != "partial" {
		t.Fatalf("%+v %+v", jv.Status, jv.Segments[2])
	}
	if c := e.charges(jv.JobID)[2]; c.Status != model.ChargeRefunded {
		t.Fatalf("镜像应按真实状态收敛为已退回：%+v", c)
	}
	e.assertMoney(jv.JobID)
}

// M4：汇总到终态与用户重做并发：重做先插入步骤，终态迁移不成功，job 继续执行重做。
func TestM4FinishRacesRedo(t *testing.T) {
	e := newEnv(t, true)
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
	e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	jv = e.run(jv.JobID, 1)
	var redoErr error
	e.store.beforeFinish = func(id int64) { // rollup 判定终态之后、写库之前，用户点了重做
		_, redoErr = e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: id, SegIndex: 2, IdempotencyKey: "r1"})
	}
	jv = e.run(jv.JobID, 1)
	if redoErr != nil {
		t.Fatalf("并发重做应成功：%v", redoErr)
	}
	if j := e.job(jv.JobID); model.JobFinal(j.Status) {
		t.Fatalf("有新的未终态步骤时不得汇总到终态：%d", j.Status)
	}
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" {
		t.Fatalf("重做完成后应成功：%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

// M5：冻结成功但读状态出错：不当成预留失败，按请求号幂等退回，文案不说「没有扣点数」。
func TestM5HoldStatusUnknownRefunded(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	e.ledger.holdUnknown = 1
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if err == nil || strings.Contains(err.Error(), "没有扣点数") {
		t.Fatalf("冻结结果未知不能说没有扣点数：%v", err)
	}
	j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1")
	if e.ledger.status(e.rFirst(j.ID, 1)) != "REFUNDED" || e.charges(j.ID)[0].Status != model.ChargeRefunded {
		t.Fatalf("应幂等退回：%s %+v", e.ledger.status(e.rFirst(j.ID, 1)), e.charges(j.ID)[0])
	}
	// 退回也失败：镜像结算待重试，job 文案说正在退回
	e2 := newEnv(t, false)
	pv2 := e2.plan(e2.seedAnalysis(10))
	e2.ledger.holdUnknown, e2.ledger.refundErrs = 1, 1
	_, _ = e2.svc.Confirm(e2.ctx, ConfirmReq{UserID: uid, PlanNo: pv2.PlanNo, PlanHash: pv2.PlanHash, IdempotencyKey: "k1"})
	j2, _ := e2.store.GetJobByRequest(e2.ctx, uid, "k1")
	if e2.charges(j2.ID)[0].Status != model.ChargeSettleFailed || strings.Contains(deref(j2.ErrorReason), "没有扣点数") {
		t.Fatalf("%+v %s", e2.charges(j2.ID)[0], deref(j2.ErrorReason))
	}
	e2.clock = e2.clock.Add(time.Minute)
	e2.svc.Tick(e2.ctx, 10)
	if e2.ledger.sum("HELD") != 0 {
		t.Fatal("补扫应退回")
	}
}

// L1：确认请求逐段预留时被回收（心跳发现 job 已不在提交中）：自己退回刚冻结的单。
func TestL1HeartbeatDetectsReap(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	e.store.onTouch = func(id int64) { // 第一次心跳前回收器动手
		e.store.onTouch = nil
		for _, x := range e.store.jobs {
			if x.ID == id {
				x.Status = model.JobFailed
			}
		}
	}
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, Stage: "all", IdempotencyKey: "k1"})
	if reasonOf(err) != "SUBMIT_TIMEOUT" {
		t.Fatalf("应提交超时：%v", err)
	}
	if e.ledger.sum("HELD") != 0 || e.ledger.holds != 1 {
		t.Fatalf("发现被回收后不再冻结、已冻结的退回：held=%d holds=%d", e.ledger.sum("HELD"), e.ledger.holds)
	}
	// 心跳刷新 updated_at：正常确认过程中不会被判为卡住
	e2 := newEnv(t, true)
	pv2 := e2.plan(e2.seedAnalysis(32, 12, 25))
	n := 0
	e2.ledger.onHold = func(string) {
		n++
		e2.clock = e2.clock.Add(3 * time.Minute) // 每段冻结都很慢：总耗时 > ReapAfter，但每段之间有心跳
	}
	_ = n
	if _, err := e2.svc.Confirm(e2.ctx, ConfirmReq{UserID: uid, PlanNo: pv2.PlanNo, PlanHash: pv2.PlanHash, Stage: "all", IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("有心跳的慢确认应成功：%v", err)
	}
}

// L2：扣费失败与退点失败分开显示。
func TestL2ConfirmFailureMoneyText(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.ledger.confirmErr = 1
	jv = e.run(jv.JobID, 2)
	if jv.Money.ChargePending != 100 || jv.Money.RefundPending != 0 || strings.Contains(jv.Money.Text, "退点") {
		t.Fatalf("扣费失败不能说退点处理中：%+v", jv.Money)
	}
	if c := e.charges(jv.JobID)[0]; !strings.Contains(ChargeMoneyText(c), "正在确认扣除") {
		t.Fatalf("%s", ChargeMoneyText(c))
	}
}

// L3：auto_retry_max=2 生效（#r1 → #r2）。
func TestL3AutoRetryMaxTwo(t *testing.T) {
	e := newEnv(t, false)
	e.rebuild("test-owner", func(c *Config) { c.AutoRetryMax = 2 })
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	jv = e.run(jv.JobID, 6)
	if e.gw.submitted("seg/01/generate#r2") != 1 || jv.Status != "succeeded" || e.ledger.holds != 1 {
		t.Fatalf("第 2 次自动重试应生效：%v %+v", e.gw.submits, jv)
	}
	if e.gw.submitted("#r3") != 0 {
		t.Fatal("不应超过上限")
	}
	if retryIndex("seg/01/generate#u1#r2") != 2 || nextRetryKey("seg/01/generate#u1#r2") != "seg/01/generate#u1#r3" || nextRetryKey("seg/01/generate") != "seg/01/generate#r1" {
		t.Fatal("重试键编号")
	}
	e.assertMoney(jv.JobID)
}

// L4：并发重做不超每 job 上限（原子自增 + 条件）。
func TestL4RedoCountAtomic(t *testing.T) {
	e := newEnv(t, true)
	e.rebuild("test-owner", func(c *Config) { c.RedoMaxPerJob = 1 })
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
	for _, k := range []string{"seg/01/generate", "seg/02/generate", "seg/03/generate"} {
		e.gw.script[k] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "failed" {
		t.Fatalf("%+v", jv)
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: i + 1, IdempotencyKey: "r" + string(rune('a'+i))})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if reasonOf(err) != "REDO_LIMIT" {
			t.Fatalf("超限应 REDO_LIMIT：%v", err)
		}
	}
	if ok != 1 || e.job(jv.JobID).RedoCount != 1 || e.ledger.holds != 4 {
		t.Fatalf("只能成功一次：ok=%d redo_count=%d holds=%d", ok, e.job(jv.JobID).RedoCount, e.ledger.holds)
	}
}

// 待确认：幂等键环境前缀（冻结进 job / 分析，改配置不影响在途）；非法前缀按空处理。
func TestKeyPrefixFrozen(t *testing.T) {
	e := newEnv(t, false)
	e.rebuild("test-owner", func(c *Config) { c.KeyPrefix = "stg-" })
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.rebuild("test-owner", func(c *Config) { c.KeyPrefix = "prod-" }) // 改配置
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || e.gw.submitted("stg-"+idemKey(jv.JobID, "seg/01/generate")) != 1 || e.gw.submitted("prod-"+idemKey(jv.JobID, "seg/01/generate")) != 0 {
		t.Fatalf("出片幂等键应带确认时冻结的前缀：%v", e.gw.submits)
	}
	if !ValidKeyPrefix("") || ValidKeyPrefix("a:b") || ValidKeyPrefix(strings.Repeat("x", 33)) {
		t.Fatal("前缀校验")
	}
	if c := (Config{KeyPrefix: "bad prefix"}).withDefaults(); c.KeyPrefix != "" {
		t.Fatal("非法前缀按空处理")
	}
}

// 待确认：分析的 max_cost 与前缀冻结进分析行，运行参数变了重提仍用冻结值。
func TestAnalysisMaxCostFrozen(t *testing.T) {
	e := newEnv(t, false)
	e.rebuild("test-owner", func(c *Config) { c.MaxCostCNY = "1.50"; c.KeyPrefix = "a-" })
	e.media.probe = mediasvc.ProbeInfo{Mime: "video/mp4", DurationS: 10, HasAudio: true}
	setupAnalysisGW(e)
	v, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "mr://" + testRid("mc")})
	if err != nil {
		t.Fatal(err)
	}
	e.rebuild("test-owner", func(c *Config) { c.MaxCostCNY = "9.00"; c.KeyPrefix = "b-" })
	a, _ := e.store.GetAnalysisByNo(e.ctx, uid, v.AnalysisNo)
	e.drainAnalysis(a.ID)
	if len(e.gw.submits) == 0 {
		t.Fatal("应调用网关")
	}
	for _, k := range e.gw.submits {
		r := e.gw.reqs[k]
		if !strings.HasPrefix(k, "a-rpl-a-") || r.Limits == nil || r.Limits.MaxCost != "1.50" {
			t.Fatalf("应使用冻结的前缀与 max_cost：%s %+v", k, r.Limits)
		}
	}
}

// 待确认：源视频地址长度、来源与归属。
func TestSourceValidation(t *testing.T) {
	e := newEnv(t, false)
	long := "https://own.test/video/7/20261113/" + strings.Repeat("a", 1100) + ".mp4"
	cases := map[string]string{
		long: "太长",
		"https://own.test/aigc/video/8/20261113/v.mp4": "不是你在本站上传", // 别人的上传目录
		"https://evil.example/v.mp4":                   "不是你在本站上传",
		"mr://abc":                                     "不是从本站上传",
		"mr://" + workRid("x"):                         "不是从本站上传", // 产物类 rid 不是用户上传
	}
	for src, want := range cases {
		_, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: src})
		if !isParamErr(err) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q 应 1003「%s」：%v", src[:min(len(src), 60)], want, err)
		}
	}
	if _, err := e.svc.CreateAnalysis(e.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://own.test/aigc/video/7/20261113/v.mp4"}); err != nil {
		t.Fatalf("本人上传应接受：%v", err)
	}
	// 没装配归属校验：http(s) 一律拒收
	e2 := newEnv(t, false)
	e2.svc.urlOwner = nil
	if _, err := e2.svc.CreateAnalysis(e2.ctx, CreateAnalysisReq{UserID: uid, SourceURL: "https://own.test/aigc/video/7/20261113/v.mp4"}); !isParamErr(err) {
		t.Fatalf("未装配归属校验应拒收：%v", err)
	}
}

// 待确认：参考图 http(s) 归属 + 拉入成 rid；rid 只认用户上传类；分辨率 / 画幅白名单。
func TestPlanRefsAndWhitelist(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(10)
	req := func(refs []string, res, ratio string) (*PlanView, error) {
		return e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: refs,
			Resolution: res, AspectRatio: ratio, Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	}
	gapCodes := func(err error) string {
		var ge *GapsError
		if !errors.As(err, &ge) {
			return "not-gaps:" + errString(err)
		}
		var cs []string
		for _, g := range ge.Gaps {
			cs = append(cs, g.Code)
		}
		return strings.Join(cs, ",")
	}
	if c := gapCodes(func() error { _, err := req([]string{testRef}, "4k", "5:4"); return err }()); c != "RESOLUTION_UNSUPPORTED,ASPECT_UNSUPPORTED" {
		t.Fatalf("白名单：%s", c)
	}
	if c := gapCodes(func() error {
		_, err := req([]string{"https://evil.example/a.png", "mr://abc", "https://own.test/aigc/image/8/20261113/a.png"}, "", "")
		return err
	}()); c != "REF_NOT_OWNED,REF_NOT_OWNED,REF_NOT_OWNED" {
		t.Fatalf("归属：%s", c)
	}
	e.media.ingests = 0
	pv, err := req([]string{"https://own.test/aigc/image/7/20261113/a.png?sign=1"}, "1080p", "9:16")
	if err != nil {
		t.Fatalf("本人上传的图应接受：%v", err)
	}
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if !strings.Contains(string(p.Refs), "-user-") || e.media.ingests != 1 || p.Resolution != "1080p" || p.AspectRatio != "9:16" {
		t.Fatalf("http 参考图应经媒体服务拉入成 rid：%s ingests=%d", p.Refs, e.media.ingests)
	}
	e.media.ingestErr = &mediasvc.Error{Status: 400, Code: "bad"}
	if c := gapCodes(func() error {
		_, err := req([]string{"https://own.test/aigc/image/7/20261113/b.png"}, "", "")
		return err
	}()); c != "REF_UNREADABLE" {
		t.Fatalf("读不了：%s", c)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// 追加：JobView 下发现签播放地址与封面；签名失败不影响视图。
func TestJobViewSignedURLs(t *testing.T) {
	e := newEnv(t, false)
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID, 3)
	sg := jv.Segments[0]
	if sg.VideoURL == "" || sg.CoverURL == "" || !strings.Contains(sg.VideoURL, sg.VideoRid) || jv.URLExpiresAt == nil {
		t.Fatalf("应带播放 / 封面地址：%+v %v", sg, jv.URLExpiresAt)
	}
	if jv.URLExpiresAt.Sub(e.clock) != time.Hour {
		t.Fatalf("默认签 1 小时：%v", jv.URLExpiresAt)
	}
	if jv.Result == nil || jv.Result.VideoURL == "" || jv.Result.CoverURL == "" {
		t.Fatalf("成片也应带地址：%+v", jv.Result)
	}
	e.signer.err = errors.New("media down")
	jv2, err := e.svc.JobView(e.ctx, uid, jv.JobID)
	if err != nil || jv2.Segments[0].VideoURL != "" || jv2.Segments[0].VideoRid == "" {
		t.Fatalf("签名失败只给 rid：%v %+v", err, jv2.Segments[0])
	}
}

// 追加：历史列表（倒序、分页、摘要、封面）。
func TestListJobs(t *testing.T) {
	e := newEnv(t, false)
	j1 := e.run(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID, 3)
	j2 := e.confirm(e.plan(e.seedAnalysis(11)), "", "k2")
	l, err := e.svc.ListJobs(e.ctx, uid, 1, 1)
	if err != nil || l.Total != 2 || len(l.Items) != 1 || l.Items[0].JobID != j2.JobID || l.Items[0].Points != 110 {
		t.Fatalf("第 1 页应是最新一条：%v %+v", err, l)
	}
	l, _ = e.svc.ListJobs(e.ctx, uid, 2, 1)
	it := l.Items[0]
	if it.JobID != j1.JobID || it.Status != "succeeded" || it.Points != 100 || it.CoverURL == "" || it.Title == "" || it.CreatedAt.IsZero() {
		t.Fatalf("摘要：%+v", it)
	}
	if l, _ := e.svc.ListJobs(e.ctx, uid+1, 1, 20); l.Total != 0 {
		t.Fatal("只看本人")
	}
	if l, _ := e.svc.ListJobs(e.ctx, uid, 1, 500); l.Size != maxListSize {
		t.Fatal("size 上限")
	}
	if _, err := e.svc.ListJobs(e.ctx, 0, 1, 1); err == nil {
		t.Fatal("未登录")
	}
	_ = xerr.Unauthorized
}

// N3：RenewAfter 永远不早于 HoldTTL − billing.LeaseDuration（任何一次续期的新到期都不早于原到期）。
func TestN3RenewAfterNeverShortens(t *testing.T) {
	for _, c := range []Config{{}, {RenewAfter: time.Minute}, {HoldTTL: 60 * time.Minute, RenewAfter: 10 * time.Minute}, {HoldTTL: 300 * time.Minute}} {
		d := c.withDefaults()
		if d.RenewAfter < d.HoldTTL-15*time.Minute {
			t.Fatalf("RenewAfter=%v HoldTTL=%v 会缩短到期", d.RenewAfter, d.HoldTTL)
		}
	}
	if d := (Config{}).withDefaults(); d.HoldTTL != 200*time.Minute || d.RenewAfter != 186*time.Minute {
		t.Fatalf("默认：%v %v", d.HoldTTL, d.RenewAfter)
	}
}

// ---------------------------------------------------------------- 第二轮复核（N1–N9）

func failedSingle(t *testing.T, e *env) *JobView {
	t.Helper()
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "failed" {
		t.Fatalf("%+v", jv)
	}
	return jv
}

// N1：终态 job 上重做，预留期间轮询器推进（终态补扫）不得退掉重做的单；重做完成后能收尾。
func TestN1RedoOnFinalJobConcurrentSweep(t *testing.T) {
	e := newEnv(t, false)
	jv := failedSingle(t, e)
	e.ledger.onHold = func(req string) {
		if strings.Contains(req, "-u1") {
			e.ledger.onHold = nil
			if j := e.job(jv.JobID); j.Status != model.JobRunning {
				t.Errorf("ClaimRedo 应在同一条语句里把 job 拉回执行中：%d", j.Status)
			}
			_ = e.svc.Advance(e.ctx, jv.JobID) // 预留进行中（HOLDING）轮询器推进
		}
	}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"}); err != nil {
		t.Fatal(err)
	}
	if st := e.ledger.status(e.rRedo(jv.JobID, 1, 1)); st != "HELD" {
		t.Fatalf("预留期间的推进不得退掉重做的单：%s", st)
	}
	// 读到旧终态的 settleJob（Advance 开头读的是重做前的行）也不得误退
	stale := e.job(jv.JobID)
	stale.Status = model.JobFailed
	_ = e.svc.settleJob(e.ctx, stale)
	if st := e.ledger.status(e.rRedo(jv.JobID, 1, 1)); st != "HELD" {
		t.Fatalf("旧终态视图下不得误退：%s", st)
	}
	if n, _ := e.store.CancelOrphanSteps(e.ctx, jv.JobID); n != 0 {
		t.Fatal("job 已在执行中：孤儿步骤清理不得碰重做步骤")
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 {
		t.Fatalf("重做应完成并收尾：%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

// N1 兜底：排队步骤的镜像已退回 → 置失败让 job 收尾（可重做）；重做预留失败把 job 还原为原终态。
func TestN1ReleasedPendingFailsAndHoldFailureReverts(t *testing.T) {
	e := newEnv(t, false)
	jv := failedSingle(t, e)
	before := e.job(jv.JobID)
	e.ledger.balance = 0
	_, _ = e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"})
	if j := e.job(jv.JobID); j.Status != model.JobFailed || j.FinishedAt == nil || !j.FinishedAt.Equal(*before.FinishedAt) || j.RedoCount != 0 {
		t.Fatalf("预留失败应还原为原终态、还回名额：%+v", j)
	}
	e.ledger.balance = -1
	jv, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟：单被外部退掉（旧版本的误退 / 人工处理）
	rid := e.rRedo(jv.JobID, 1, 2)
	_, _ = e.ledger.Refund(e.ctx, uid, rid, "x")
	for _, c := range e.charges(jv.JobID) {
		if c.RequestID == rid {
			_, _ = e.store.UpdateCharge(e.ctx, c.ID, []int8{model.ChargeHeld}, map[string]any{"status": model.ChargeRefunded})
		}
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "failed" || !jv.Segments[0].Redoable || e.gw.submitted("#u2") != 0 {
		t.Fatalf("钱已不在的排队步骤应置失败、job 收尾且可再重做：%+v %+v", jv.Status, jv.Segments[0])
	}
}

// N2：冻结成功但写镜像失败 → 按失败退回并中止；镜像卡在待预留时推进按真实状态修正。
func TestN2MirrorWriteFailureAndRepair(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	e.store.chargeWriteErr = 1
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"}); err == nil {
		t.Fatal("写镜像失败应按失败处理")
	}
	j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1")
	if e.ledger.sum("HELD") != 0 || j.Status != model.JobFailed {
		t.Fatalf("应退回并中止：held=%d job=%d", e.ledger.sum("HELD"), j.Status)
	}
	// 修正：镜像停在待预留、预留单其实是 HELD（旧版本 / 崩溃留下）
	e2 := newEnv(t, false)
	jv := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1")
	c := e2.charges(jv.JobID)[0]
	_, _ = e2.store.UpdateCharge(e2.ctx, c.ID, []int8{model.ChargeHeld}, map[string]any{"status": model.ChargePending})
	jv = e2.run(jv.JobID, 1)
	if e2.gw.submitted("seg/01/generate") != 0 {
		t.Fatal("回收阈值内不动、也不出片")
	}
	e2.clock = e2.clock.Add(6 * time.Minute)
	jv = e2.run(jv.JobID, 3)
	if jv.Status != "succeeded" || e2.charges(jv.JobID)[0].Status != model.ChargeConfirmed {
		t.Fatalf("镜像应修正为已预留并照常出片：%+v %+v", jv, e2.charges(jv.JobID)[0])
	}
	// 没有预留单：记预留失败，绝不当成不计费放行
	e3 := newEnv(t, false)
	jv3 := e3.confirm(e3.plan(e3.seedAnalysis(10)), "", "k1")
	c3 := e3.charges(jv3.JobID)[0]
	_, _ = e3.store.UpdateCharge(e3.ctx, c3.ID, []int8{model.ChargeHeld}, map[string]any{"status": model.ChargePending})
	delete(e3.ledger.res, c3.RequestID)
	e3.clock = e3.clock.Add(6 * time.Minute)
	jv3 = e3.run(jv3.JobID, 3)
	if e3.gw.submitted("seg/01/generate") != 0 || jv3.Status != "failed" || e3.charges(jv3.JobID)[0].Status != model.ChargeHoldFailed {
		t.Fatalf("没有预留单不得出片：%+v %+v", jv3.Status, e3.charges(jv3.JobID)[0])
	}
}

// N4：并发确认误报 Gone → 查状态是已扣除，按成功交付；要退的单已扣除 → 镜像收敛为已扣除，不无限补扫。
func TestN4ConfirmRaceAndRefundOnConfirmed(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.ledger.confirmRace = 1
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Segments[0].VideoRid == "" || e.charges(jv.JobID)[0].Status != model.ChargeConfirmed {
		t.Fatalf("并发确认误报 Gone 应按成功交付：%+v", jv)
	}
	e.assertMoney(jv.JobID)

	e2 := newEnv(t, false)
	j2 := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1")
	e2.run(j2.JobID, 1)
	e2.ledger.res[e2.rFirst(j2.JobID, 1)].status = "CONFIRMED" // 状态不一致：单已扣除
	e2.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	e2.run(j2.JobID, 3)
	if c := e2.charges(j2.JobID)[0]; c.Status != model.ChargeConfirmed || !strings.Contains(deref(c.Reason), "人工核对") {
		t.Fatalf("要退的单已扣除应收敛为已扣除：%+v", c)
	}
	e2.clock = e2.clock.Add(time.Hour)
	if due, _ := e2.store.DueJobs(e2.ctx, 10); len(due) != 0 {
		t.Fatalf("不应无限补扫：%v", due)
	}
}

// N5：有未结镜像时补扫间隔上限 2 分钟（结束很久也一样）。
func TestN5OpenChargeBackoffCap(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	e.ledger.refundErrs = 1000
	e.run(jv.JobID, 3)
	e.clock = e.clock.Add(10 * time.Hour)
	e.svc.Tick(e.ctx, 10)
	j := e.job(jv.JobID)
	if j.NextRunAt == nil || j.NextRunAt.Sub(e.clock) > 2*time.Minute || j.NextRunAt.Sub(e.clock) < 15*time.Second {
		t.Fatalf("补扫间隔应在 15 秒 ~ 2 分钟：%v", j.NextRunAt.Sub(e.clock))
	}
}

// N6：终态 job 上卡住的重做预留步骤也会被回收并还回名额。
func TestN6ReapHoldingOnFinalJob(t *testing.T) {
	e := newEnv(t, false)
	jv := failedSingle(t, e)
	// 模拟旧版本 / 崩溃：终态 job 上留下 HOLDING 步骤与待预留镜像，名额已占
	sk := genKey(1, "#u1")
	rid := e.rRedo(jv.JobID, 1, 1)
	_, _, _ = e.store.AddRedo(e.ctx, &model.Step{JobID: jv.JobID, StepKey: sk, Kind: model.StepKindGateway, SegIndex: 1, Status: model.StepHolding,
		IdemKey: idemKey(jv.JobID, sk), BillingRequestID: ptr(rid)},
		&model.Charge{JobID: jv.JobID, UserID: uid, SegIndex: 1, Stage: model.ChargeStageRedo, RequestID: rid, ClientKey: ptr("r1"), Seconds: 10})
	for _, x := range e.store.jobs {
		x.RedoCount = 1
	}
	e.clock = e.clock.Add(6 * time.Minute)
	_ = e.svc.Advance(e.ctx, jv.JobID)
	if st := e.step(jv.JobID, sk); st.Status != model.StepCanceled || deref(st.ErrorCode) != codeHoldAbandoned {
		t.Fatalf("应回收：%+v", st)
	}
	if e.job(jv.JobID).RedoCount != 0 {
		t.Fatal("应还回名额")
	}
	_ = e.svc.Advance(e.ctx, jv.JobID)
	for _, c := range e.charges(jv.JobID) {
		if model.ChargeOpen(c.Status) {
			t.Fatalf("镜像应结清：%+v", c)
		}
	}
}

// N8：继续中途失败的退点待重试不拖到终态：待继续期间就补退。
func TestN8ContinueRefundRetriedWhileWaiting(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.run(e.confirm(pv, "first", "k1").JobID, 3)
	e.ledger.balance = 150 // 够第 2 段（140），不够第 3 段
	e.ledger.refundErrs = 1
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1"}); err == nil {
		t.Fatal("应点数不足")
	}
	c2 := e.rRest(jv.JobID, 2, 1)
	if e.ledger.status(c2) != "HELD" || e.job(jv.JobID).Status != model.JobWaiting {
		t.Fatalf("退回失败：单仍冻结、job 待继续：%s", e.ledger.status(c2))
	}
	for i := 0; i < 5 && e.ledger.status(c2) == "HELD"; i++ {
		e.clock = e.clock.Add(time.Minute)
		e.svc.Tick(e.ctx, 10)
	}
	if e.ledger.status(c2) != "REFUNDED" || e.job(jv.JobID).Status != model.JobWaiting {
		t.Fatalf("待继续期间就应补退（不等终态）：%s job=%d", e.ledger.status(c2), e.job(jv.JobID).Status)
	}
}
