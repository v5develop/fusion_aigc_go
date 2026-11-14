package core

import (
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/quote"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"
)

func reasonOf(err error) string { return xerr.ReasonOf(err) }

func TestSingleSegmentConfirmAndSucceed(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(12.3)
	pv := e.plan(a)
	if pv.Cost.Total != 130 || pv.Cost.DefaultStage != quote.StageAll || pv.ConfirmButton != "确认并生成（130 点）" || !pv.Timeline.Single {
		t.Fatalf("单段方案卡不对：%+v", pv.Cost)
	}
	jv := e.confirm(pv, "", "k1")
	if jv.Status != "queued" || jv.Money.Held != 130 {
		t.Fatalf("确认后应排队且预留 130：%+v", jv)
	}
	cs := e.charges(jv.JobID)
	if len(cs) != 1 || cs[0].RequestID != e.rFirst(jv.JobID, 1) || cs[0].Status != model.ChargeHeld {
		t.Fatalf("预留镜像不对：%+v", cs[0])
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 130 || jv.Segments[0].VideoRid == "" {
		t.Fatalf("应成功并扣除 130：%+v", jv)
	}
	req := e.gw.reqs[idemKey(jv.JobID, "seg/01/generate")]
	in := req.Input.(map[string]any)
	if req.Operation != "video.generate" || in["duration_s"] != 13 || req.Labels["caller_biz"] != "replicate" || req.Constraints == nil {
		t.Fatalf("网关请求不对：%+v", req)
	}
	if refs := in["reference_videos"].([]aigateway.MediaRef); refs[0].URI != "mr://src-rid" {
		t.Fatalf("单段应直接用原片：%+v", refs)
	}
	e.assertMoney(jv.JobID)
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if p.Status != model.PlanConfirmed || *p.JobID != jv.JobID {
		t.Fatalf("方案应已确认：%+v", p)
	}
}

func TestConfirmIdempotentAndGuards(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	// 价变：页面看到 90，实际 100 → 409，不动钱
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k0", ExpectedPoints: 90})
	if reasonOf(err) != "QUOTE_CHANGED" || !strings.Contains(err.Error(), "90 → 100") || e.ledger.holds != 0 {
		t.Fatalf("价变应 409：%v holds=%d", err, e.ledger.holds)
	}
	// 指纹不一致 → 409
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: "x", IdempotencyKey: "k0"}); reasonOf(err) != "PLAN_STALE" {
		t.Fatalf("指纹不一致应 STALE：%v", err)
	}
	// 缺幂等键
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash}); err == nil {
		t.Fatal("缺 Idempotency-Key 应拒绝")
	}
	j1 := e.confirm(pv, "all", "k1")
	j2 := e.confirm(pv, "all", "k1") // 同键重复点击
	j3 := e.confirm(pv, "all", "k2") // 换键：方案已确认，返回已有 job
	if j1.JobID != j2.JobID || j1.JobID != j3.JobID || e.ledger.holds != 1 {
		t.Fatalf("应幂等：%d %d %d holds=%d", j1.JobID, j2.JobID, j3.JobID, e.ledger.holds)
	}
	// 别人的 job 看不到
	if _, err := e.svc.JobView(e.ctx, uid+1, j1.JobID); err == nil {
		t.Fatal("越权应返回找不到")
	}
	// 过期方案
	e.clock = e.clock.Add(8 * 24 * time.Hour)
	pv2 := e.plan(e.seedAnalysis(11))
	e.clock = e.clock.Add(8 * 24 * time.Hour)
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv2.PlanNo, PlanHash: pv2.PlanHash, IdempotencyKey: "k9"}); reasonOf(err) != "PLAN_EXPIRED" {
		t.Fatalf("过期应 409：%v", err)
	}
}

func TestConfirmHoldFailureRollsBack(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25)) // 3 段：13+14+8 秒（含拼接处）
	e.ledger.balance = 200                   // 只够第 1 段
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, Stage: "all", IdempotencyKey: "k1"})
	if err == nil || !strings.Contains(err.Error(), "点数不足") {
		t.Fatalf("应点数不足：%v", err)
	}
	if e.ledger.balance != 200 || e.ledger.sum("HELD") != 0 {
		t.Fatalf("已预留的段必须全部退回：balance=%d held=%d", e.ledger.balance, e.ledger.sum("HELD"))
	}
	j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1")
	if j.Status != model.JobFailed {
		t.Fatalf("job 应失败：%d", j.Status)
	}
	for _, c := range e.charges(j.ID) {
		if c.Status != model.ChargeRefunded && c.Status != model.ChargeHoldFailed {
			t.Fatalf("镜像应是已退回 / 预留失败：%+v", c)
		}
	}
	// 方案回到可确认：充值后换个键再确认成功
	e.ledger.balance = 10000
	jv := e.confirm(pv, "all", "k2")
	if jv.JobID == j.ID || jv.Money.Held != pv.Cost.Total {
		t.Fatalf("充值后应能重新确认：%+v", jv)
	}
}

func TestStagedFirstThenContinue(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	if pv.Cost.DefaultStage != quote.StageFirst || !strings.HasPrefix(pv.ConfirmButton, "先做第 1 段") || pv.Cost.RestText == "" {
		t.Fatalf("多段默认先做第 1 段：%+v / %s", pv.Cost, pv.ConfirmButton)
	}
	first, rest := pv.Cost.First, pv.Cost.Rest
	jv := e.confirm(pv, "", "k1")
	if !jv.Staged || jv.Money.Held != first || len(e.charges(jv.JobID)) != 1 {
		t.Fatalf("只应预留第 1 段：%+v", jv)
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "waiting" || jv.Decision == nil || jv.Decision.Kind != "continue" || jv.Decision.Options[0].Points != rest {
		t.Fatalf("第 1 段好后应待继续并给决策卡：%+v %+v", jv, jv.Decision)
	}
	if e.gw.submitted("seg/02/generate") != 0 {
		t.Fatal("未继续前其余段绝不出片")
	}
	if jv.Segments[1].State != "not_started" {
		t.Fatalf("其余段应显示未开始：%+v", jv.Segments[1])
	}
	// 指纹不一致
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: "x", IdempotencyKey: "c1"}); reasonOf(err) != "PLAN_STALE" {
		t.Fatalf("应 STALE：%v", err)
	}
	// 价变
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1", ExpectedPoints: rest - 1}); reasonOf(err) != "QUOTE_CHANGED" {
		t.Fatalf("应价变：%v", err)
	}
	jv, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1", ExpectedPoints: rest})
	if err != nil || jv.Status != "running" {
		t.Fatalf("继续应成功：%v %+v", err, jv)
	}
	if again, _ := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1"}); again.JobID != jv.JobID {
		t.Fatal("重复点击应返回同一结果")
	}
	if e.ledger.status(e.rRest(jv.JobID, 2, 1)) != "HELD" || e.ledger.status(e.rRest(jv.JobID, 3, 1)) != "HELD" {
		t.Fatal("其余段应按 -c1 请求号预留")
	}
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" || jv.Money.Confirmed != first+rest {
		t.Fatalf("全部完成应扣 %d：%+v", first+rest, jv.Money)
	}
	if len(e.cutter.cuts) != 3 {
		t.Fatalf("每段应切一次：%v", e.cutter.cuts)
	}
	e.assertMoney(jv.JobID)
}

func TestContinueHoldFailureBackToWaitingNoReuse(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.confirm(pv, "first", "k1")
	jv = e.run(jv.JobID, 3)
	e.ledger.balance = 10 // 不够其余段
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1"}); err == nil || !strings.Contains(err.Error(), "点数不足") {
		t.Fatalf("应点数不足：%v", err)
	}
	j, _ := e.store.GetJob(e.ctx, jv.JobID)
	if j.Status != model.JobWaiting || j.ContinueRequestID != nil || e.ledger.sum("HELD") != 0 || e.ledger.balance != 10 {
		t.Fatalf("应回到待继续且不留冻结：status=%d held=%d bal=%d", j.Status, e.ledger.sum("HELD"), e.ledger.balance)
	}
	e.ledger.balance = 10000
	jv2, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c2"})
	if err != nil || jv2.Status != "running" {
		t.Fatalf("充值后继续应成功：%v", err)
	}
	if e.ledger.status(e.rRest(jv.JobID, 2, 2)) != "HELD" {
		t.Fatal("第二次继续必须用新请求号 -c2（不复用已退回的单）")
	}
	jv2 = e.run(jv.JobID, 4)
	if jv2.Status != "succeeded" {
		t.Fatalf("%+v", jv2)
	}
	e.assertMoney(jv.JobID)
}

func TestContinueDeadlineAndStop(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.run(e.confirm(pv, "first", "k1").JobID, 3)
	if jv.Decision.RemainingText != "还剩 24 小时" {
		t.Fatalf("决策卡倒计时：%+v", jv.Decision)
	}
	e.clock = e.clock.Add(25 * time.Hour)
	jv = e.run(jv.JobID, 1)
	if jv.Status != "succeeded" || !strings.Contains(jv.Message, "只保留了第 1 段，没有收费") || jv.Money.Confirmed != pv.Cost.First {
		t.Fatalf("到期应以仅第 1 段收尾：%+v", jv)
	}
	if jv.Segments[1].StateLabel != "未做" || len(e.charges(jv.JobID)) != 1 {
		t.Fatalf("其余段应未做且从未预留：%+v", jv.Segments)
	}
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1"}); err == nil {
		t.Fatal("收尾后不能再继续")
	}
	e.assertMoney(jv.JobID)

	// 用户主动「不继续了」
	e2 := newEnv(t, true)
	pv2 := e2.plan(e2.seedAnalysis(32, 12, 25))
	j2 := e2.run(e2.confirm(pv2, "first", "k1").JobID, 3)
	j2, err := e2.svc.Stop(e2.ctx, uid, j2.JobID)
	if err != nil || j2.Status != "succeeded" || !strings.Contains(j2.Message, "没有再收费") {
		t.Fatalf("不继续应收尾：%v %+v", err, j2)
	}
}

func TestAutoContinuePref(t *testing.T) {
	e := newEnv(t, true)
	_ = e.svc.SetAutoContinue(e.ctx, uid, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	if pv.Cost.DefaultStage != quote.StageAll {
		t.Fatal("勾了「以后直接全部做」默认应全部")
	}
	// 显式先做第 1 段时，第 1 段好后自动继续
	jv := e.run(e.confirm(pv, "first", "k1").JobID, 3)
	if jv.Status != "running" && jv.Status != "succeeded" {
		t.Fatalf("应自动继续：%+v", jv)
	}
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" || jv.Money.Confirmed != pv.Cost.Total {
		t.Fatalf("%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

func TestAutoRetryOnceThenSucceed(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.confirm(pv, "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	jv = e.run(jv.JobID, 2)
	if jv.Segments[0].State != "retrying" && jv.Segments[0].State != "running" {
		t.Fatalf("应在自动重试：%+v", jv.Segments[0])
	}
	if e.ledger.status(e.rFirst(jv.JobID, 1)) != "HELD" {
		t.Fatal("自动重试期间预留单不动")
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 || e.ledger.holds != 1 {
		t.Fatalf("重试成功只扣一次：%+v holds=%d", jv, e.ledger.holds)
	}
	if e.gw.submitted("seg/01/generate#r1") != 1 {
		t.Fatal("自动重试必须用新幂等键 #r1")
	}
	e.assertMoney(jv.JobID)
}

func TestAutoRetryFailsThenRefundAndRedo(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.confirm(pv, "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	jv = e.run(jv.JobID, 5)
	if jv.Status != "failed" || jv.Money.Refunded != 100 || jv.Segments[0].MoneyText != "已退回 100 点" {
		t.Fatalf("重试仍失败应退回：%+v %+v", jv, jv.Segments[0])
	}
	if jv.Segments[0].Text != "这一段等了太久，已停止" || jv.Decision == nil || jv.Decision.Kind != "redo" {
		t.Fatalf("失败文案 / 决策卡不对：%+v %+v", jv.Segments[0], jv.Decision)
	}
	e.assertMoney(jv.JobID)
	// 单段重做：新预留 -u1，重复点击幂等
	jv, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"})
	if err != nil || jv.Status != "running" {
		t.Fatalf("重做应成功：%v %+v", err, jv)
	}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"}); err != nil || e.ledger.holds != 2 {
		t.Fatalf("重做重复点击应幂等：%v holds=%d", err, e.ledger.holds)
	}
	if e.ledger.status(e.rRedo(jv.JobID, 1, 1)) != "HELD" || e.ledger.status(e.rFirst(jv.JobID, 1)) != "REFUNDED" {
		t.Fatal("重做用新预留单，原失败段已退回不再动")
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 || jv.Money.Refunded != 100 {
		t.Fatalf("重做成功：%+v", jv.Money)
	}
	e.assertMoney(jv.JobID)
}

func TestContentPolicyNoAutoRetry(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "failed" || e.gw.submitted("#r1") != 0 || jv.Money.Refunded != 100 {
		t.Fatalf("内容策略不自动重试、直接退：%+v", jv)
	}
	if !strings.Contains(jv.Segments[0].Text, "没能通过生成方的检查") || jv.Segments[0].RedoLabel != "修改后重做" {
		t.Fatalf("%+v", jv.Segments[0])
	}
	// UNKNOWN 也不自动重试
	e2 := newEnv(t, false)
	j2 := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1")
	e2.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusUnknown}}
	j2 = e2.run(j2.JobID, 3)
	if j2.Status != "failed" || e2.gw.submitted("#r1") != 0 || e2.ledger.sum("REFUNDED") != 100 {
		t.Fatalf("UNKNOWN 不重试、退点：%+v", j2)
	}
}

func TestPartialSuccessAndRedoLimit(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.confirm(pv, "all", "k1")
	e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/02/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "partial" || jv.SegDone != 2 || jv.SegFailed != 1 {
		t.Fatalf("应部分成功：%+v", jv)
	}
	if !strings.Contains(jv.Money.Text, "退回") || jv.Decision.Options[0].SegIndex != 2 {
		t.Fatalf("部分成功文案 / 决策卡：%+v %+v", jv.Money, jv.Decision)
	}
	e.assertMoney(jv.JobID)
	// 成功段不能重做
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "x"}); reasonOf(err) != "SEGMENT_OK" {
		t.Fatalf("成功段不能重做：%v", err)
	}
	// 连续重做 3 次都失败（每次含 1 次自动重试），第 4 次被拒
	for n := 1; n <= 3; n++ {
		tag := "#u" + string(rune('0'+n))
		e.gw.script["seg/02/generate"+tag] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
		e.gw.script["seg/02/generate"+tag+"#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
		if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r" + tag}); err != nil {
			t.Fatalf("第 %d 次重做：%v", n, err)
		}
		jv = e.run(jv.JobID, 6)
		if jv.Status != "partial" {
			t.Fatalf("第 %d 次重做失败后应回到部分成功：%+v", n, jv)
		}
	}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r4"}); reasonOf(err) != "REDO_LIMIT" {
		t.Fatalf("超过重做上限应拒绝：%v", err)
	}
	if jv.Segments[1].Redoable {
		t.Fatal("到上限后不再显示重做按钮")
	}
	e.assertMoney(jv.JobID)
	if e.ledger.sum("CONFIRMED") != pv.Cost.Lines[0].Points+pv.Cost.Lines[2].Points {
		t.Fatal("只为成功段付费")
	}
}

func TestDeliverBlockedWhenReservationGone(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.run(jv.JobID, 1)                     // 提交到网关
	e.ledger.expire(e.rFirst(jv.JobID, 1)) // 预留到期被回收
	jv = e.run(jv.JobID, 2)
	if jv.Status != "failed" || jv.Segments[0].VideoRid != "" || e.ledger.sum("CONFIRMED") != 0 {
		t.Fatalf("预留失效不得交付：%+v", jv)
	}
	if !strings.Contains(jv.Segments[0].Text, "预留已失效") {
		t.Fatalf("%+v", jv.Segments[0])
	}
}

func TestRefundFailureReconciles(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	e.ledger.refundErrs = 1
	e.run(jv.JobID, 2)
	jv, _ = e.svc.JobView(e.ctx, uid, jv.JobID)
	// 第一次退回失败：镜像结算待重试；第二轮推进（终态 settleJob）补退
	cs := e.charges(jv.JobID)
	if cs[0].Status != model.ChargeRefunded {
		jv2, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
		if jv2.Segments[0].MoneyText != "退点正在处理中，一般 5 分钟内到账" {
			t.Fatalf("退回未成功时不能说已退回：%+v", jv2.Segments[0])
		}
	}
	jv = e.run(jv.JobID, 2)
	if e.charges(jv.JobID)[0].Status != model.ChargeRefunded || e.ledger.status(e.rFirst(jv.JobID, 1)) != "REFUNDED" {
		t.Fatalf("对账应补退：%+v", e.charges(jv.JobID)[0])
	}
	e.assertMoney(jv.JobID)
}

func TestConfirmFailureRetriedByReconcile(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.ledger.confirmErr = 1
	jv = e.run(jv.JobID, 2)
	if jv.Status == "succeeded" {
		t.Fatal("确认扣除失败前不得置成功")
	}
	jv = e.run(jv.JobID, 2)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 {
		t.Fatalf("下一轮应补确认：%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

func TestReapSubmittingAfterCrash(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	// 模拟确认接口在逐段预留中途崩溃：job 停在提交中，第 1 段已冻结，第 2 段镜像待预留
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	in, _ := e.svc.jobInput(e.ctx, p)
	j, _, _ := e.store.CreateJob(e.ctx, &model.Job{UserID: uid, RequestID: "crash", PlanID: p.ID, PlanHash: p.PlanHash, Status: model.JobSubmitting, SegTotal: 3, Input: []byte(`{}`)},
		func(id int64) (string, []*model.Step, []*model.Charge) {
			return "t", nil, []*model.Charge{
				{JobID: id, UserID: uid, SegIndex: 1, Stage: "all", RequestID: reqFirst("", id, 1), Seconds: in.Segments[0].GenDuration},
				{JobID: id, UserID: uid, SegIndex: 2, Stage: "all", RequestID: reqFirst("", id, 2), Seconds: in.Segments[1].GenDuration}}
		})
	_, _ = e.store.ConfirmPlan(e.ctx, p.ID, j.ID)
	_, _ = e.ledger.Hold(e.ctx, e.svc.charge(uid, e.rFirst(j.ID, 1), in.ModelGroup, in.Resolution, in.Segments[0].GenDuration))
	_ = e.svc.Advance(e.ctx, j.ID)
	if cur, _ := e.store.GetJob(e.ctx, j.ID); cur.Status != model.JobSubmitting {
		t.Fatal("回收阈值内不动")
	}
	e.clock = e.clock.Add(6 * time.Minute)
	_ = e.svc.Advance(e.ctx, j.ID)
	cur, _ := e.store.GetJob(e.ctx, j.ID)
	if cur.Status != model.JobFailed || e.ledger.sum("HELD") != 0 {
		t.Fatalf("应回收：status=%d held=%d", cur.Status, e.ledger.sum("HELD"))
	}
	if p2, _ := e.store.GetPlan(e.ctx, p.ID); p2.Status != model.PlanReady {
		t.Fatal("方案应回到可确认")
	}
}

func TestResumeAfterCrashInDelivering(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.run(jv.JobID, 1)
	// 模拟：网关已成功、步骤进了 DELIVERING，进程在确认扣除前崩溃
	for _, st := range e.steps(jv.JobID) {
		_, _ = e.store.UpdateStep(e.ctx, st.ID, model.StepRunning, map[string]any{"status": model.StepDelivering, "output": []byte(`{"videoRid":"v1"}`)})
	}
	jv = e.run(jv.JobID, 1)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 || e.gw.submitted("seg/01/generate") != 1 {
		t.Fatalf("续跑应确认扣除且不重复提交：%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

func TestSubmitTemporaryErrorKeepsRunningSameKey(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.submitErr = &aigateway.Error{HTTPStatus: 503, Code: aigateway.CodeUnavailable}
	jv = e.run(jv.JobID, 2)
	if jv.Segments[0].State != "running" {
		t.Fatalf("暂时性错误应保持生成中：%+v", jv.Segments[0])
	}
	e.gw.submitErr = nil
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || e.gw.submitted("seg/01/generate") != 1 {
		t.Fatalf("恢复后同键提交一次：%+v", jv)
	}
	// 网关拒收（4xx）：不重试、退点
	e2 := newEnv(t, false)
	j2 := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1")
	e2.gw.submitErr = &aigateway.Error{HTTPStatus: 422, Code: aigateway.CodeValidationFailed}
	j2 = e2.run(j2.JobID, 2)
	if j2.Status != "failed" || e2.ledger.sum("REFUNDED") != 100 || j2.Segments[0].Redoable {
		t.Fatalf("网关拒收应失败退点且不开放重做：%+v", j2)
	}
}

func TestMaxInFlight(t *testing.T) {
	e := newEnv(t, true)
	cfg := DefaultConfig()
	cfg.Limits.SegCapSec = 15
	pv := e.plan(e.seedAnalysis(79, 13, 26, 39, 52, 65)) // 6 段
	jv := e.confirm(pv, "all", "k1")
	if jv.SegTotal != 6 {
		t.Fatalf("应 6 段：%d", jv.SegTotal)
	}
	_ = e.svc.Advance(e.ctx, jv.JobID)
	running := 0
	for _, st := range e.steps(jv.JobID) {
		if st.Status == model.StepRunning {
			running++
		}
	}
	if running != 3 {
		t.Fatalf("同时在途应 ≤3：%d", running)
	}
	jv = e.run(jv.JobID, 8)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

func TestBillingOffNoCharge(t *testing.T) {
	e := newEnv(t, false)
	e.ledger.off = true
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || e.charges(jv.JobID)[0].Status != model.ChargeFree {
		t.Fatalf("不计费档也能出片：%+v", jv)
	}
}
