package core

import (
	"fmt"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
)

// 三段全部成功的场景（带合成 + 入库），返回各段视频 rid。
func refineBase(t *testing.T) (*env, *memOps, *JobView, []string) {
	e, ops, _ := autoDeliveryEnv(t, nil)
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1").JobID, 6)
	if jv.Status != "succeeded" || len(jv.Segments) != 3 {
		t.Fatalf("前置：三段应全部成功：%s", jv.Status)
	}
	rids := make([]string, 3)
	for i, sg := range jv.Segments {
		rids[i] = sg.VideoRid
		if !sg.Refinable || sg.Redoable || sg.RefineLabel != fmt.Sprintf("精修这一段 · %d 点", sg.Points) || sg.Points <= 0 {
			t.Fatalf("已完成段应可精修（不可重做）：%+v", sg)
		}
	}
	return e, ops, jv, rids
}

func deliveryIdx(ops *memOps, jobID int64) map[string]bool {
	out := map[string]bool{}
	for _, d := range ops.dels {
		if d.JobID == jobID {
			out[fmt.Sprintf("%s/%d/v%d", d.Kind, d.SegIndex, d.Version)] = true
		}
	}
	return out
}

func TestRefineRequiresNote(t *testing.T) {
	for _, note := range []string{"", "   ", "\n\t"} {
		t.Run(fmt.Sprintf("%q", note), func(t *testing.T) {
			e, _, jv, _ := refineBase(t)
			holds := e.ledger.holds
			before, _ := e.store.GetJob(e.ctx, jv.JobID)
			_, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: note})
			if reasonOf(err) != "SEGMENT_OK" {
				t.Fatalf("无备注精修应 SEGMENT_OK：%v", err)
			}
			after, _ := e.store.GetJob(e.ctx, jv.JobID)
			if e.ledger.holds != holds || after.RedoCount != before.RedoCount || after.Status != model.JobSucceeded {
				t.Fatalf("拒收不得预留 / 占名额 / 动状态：holds=%d→%d redo=%d→%d status=%d", holds, e.ledger.holds, before.RedoCount, after.RedoCount, after.Status)
			}
		})
	}
}

func TestRefineSuccessReplacesVideoAndRecomposes(t *testing.T) {
	e, ops, jv, rids := refineBase(t)
	docBefore := resultDoc(t, e, jv.JobID)
	jv2, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: "  动作再大一点 ", ExpectedPoints: jv.Segments[1].Points})
	if err != nil {
		t.Fatal(err)
	}
	// 精修中：任务在途、这一段「精修中」、旧视频仍可播放、不能再点精修；其它段不动
	sg := jv2.Segments[1]
	if jv2.Status != "running" || sg.State != "refining" || sg.StateLabel != "精修中" || sg.VideoRid != rids[1] || sg.Refinable || sg.Redoable {
		t.Fatalf("精修中视图不对：%s %+v", jv2.Status, sg)
	}
	if jv2.Segments[0].State != "succeeded" || jv2.SegDone != 3 {
		t.Fatalf("其它段不受影响：%+v", jv2.Segments)
	}
	jv3 := e.run(jv.JobID, 6)
	if jv3.Status != "succeeded" || jv3.Segments[1].State != "succeeded" || jv3.Segments[1].Redos != 1 {
		t.Fatalf("精修成功：%s %+v", jv3.Status, jv3.Segments[1])
	}
	if jv3.Segments[1].VideoRid == rids[1] || jv3.Segments[0].VideoRid != rids[0] || jv3.Segments[2].VideoRid != rids[2] {
		t.Fatalf("只有第 2 段换成新视频：%v → %v", rids, []string{jv3.Segments[0].VideoRid, jv3.Segments[1].VideoRid, jv3.Segments[2].VideoRid})
	}
	// 其它段没有重新生成；精修步骤提示词带备注
	for _, k := range []string{"seg/01/generate", "seg/03/generate", "seg/02/generate"} {
		if n := e.gw.submitted(k); n != 1 && k != "seg/02/generate" {
			t.Fatalf("%s 不应重新生成：%d 次", k, n)
		}
	}
	if got := promptOf(t, e, "seg/02/generate#u1"); !strings.HasSuffix(got, "\n补充要求：动作再大一点") {
		t.Fatalf("精修提示词：%q", got)
	}
	if got := promptOf(t, e, "seg/02/generate"); strings.Contains(got, "补充要求") {
		t.Fatalf("原步骤不得带备注：%q", got)
	}
	// 成片按新视频重新合成
	docAfter := resultDoc(t, e, jv.JobID)
	if docAfter.Basis == docBefore.Basis || docAfter.VideoRid == docBefore.VideoRid || docAfter.Kind != ResultFull || len(docAfter.Missing) != 0 {
		t.Fatalf("应重新合成完整成片：%+v → %+v", docBefore, docAfter)
	}
	// 钱：原段扣费不动，精修另扣一笔；任务终态无冻结
	if jv3.Money.Confirmed != jv.Money.Confirmed+jv.Segments[1].Points || jv3.Money.Refunded != 0 {
		t.Fatalf("精修成功应多扣 100：%+v", jv3.Money)
	}
	e.assertMoney(jv.JobID)
	// 入库：新版本段与新成片另入一行，旧的保留
	k := deliveryIdx(ops, jv.JobID)
	for _, want := range []string{"segment/1/v0", "segment/2/v0", "segment/3/v0", "final/0/v0", "segment/2/v1", "final/0/v1"} {
		if !k[want] {
			t.Fatalf("缺入库行 %s：%v", want, k)
		}
	}
	if len(k) != 6 {
		t.Fatalf("入库行数：%v", k)
	}
	// 手动保存第 2 段命中新版本这一行（不报错、不新增）
	if _, err := e.svc.SaveSegment(e.ctx, uid, jv.JobID, 2); err != nil || len(deliveryIdx(ops, jv.JobID)) != 6 {
		t.Fatalf("保存第 2 段：%v", err)
	}
}

func TestRefineFailureKeepsOldVideo(t *testing.T) {
	e, ops, jv, rids := refineBase(t)
	docBefore := resultDoc(t, e, jv.JobID)
	delsBefore := len(ops.dels)
	e.gw.script["seg/02/generate#u1"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/02/generate#u1#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: "换个镜头"}); err != nil {
		t.Fatal(err)
	}
	jv2 := e.run(jv.JobID, 8)
	if e.gw.submitted("seg/02/generate#u1#r1") != 1 {
		t.Fatal("精修自己带一次自动重试")
	}
	if jv2.Status != "succeeded" || jv2.SegFailed != 0 || jv2.SegDone != 3 {
		t.Fatalf("精修失败不得拖垮任务状态：%s done=%d failed=%d", jv2.Status, jv2.SegDone, jv2.SegFailed)
	}
	sg := jv2.Segments[1]
	if sg.State != "succeeded" || sg.VideoRid != rids[1] || sg.Redoable || sg.Text == "" || sg.MoneyText != fmt.Sprintf("已退回 %d 点", sg.Points) {
		t.Fatalf("精修失败：仍是已完成、旧视频、有说明与退点提示：%+v", sg)
	}
	if !sg.Refinable || sg.Redos != 1 {
		t.Fatalf("失败的精修也占一次名额，仍可再精修：%+v", sg)
	}
	if jv2.Decision != nil {
		t.Fatalf("没有失败段，不出重做决策卡：%+v", jv2.Decision)
	}
	// 成片不变、没有占位、没有重新合成；不新增入库
	docAfter := resultDoc(t, e, jv.JobID)
	if docAfter.Basis != docBefore.Basis || docAfter.VideoRid != docBefore.VideoRid || docAfter.Kind != ResultFull || len(docAfter.Missing) != 0 {
		t.Fatalf("成片应保持原样：%+v → %+v", docBefore, docAfter)
	}
	if len(ops.dels) != delsBefore {
		t.Fatalf("精修失败不得新增入库：%d → %d", delsBefore, len(ops.dels))
	}
	// 钱：精修预留退回，原扣费不变
	if e.ledger.status(e.rRedo(jv.JobID, 2, 1)) != "REFUNDED" || jv2.Money.Confirmed != jv.Money.Confirmed || jv2.Money.Refunded != jv.Segments[1].Points {
		t.Fatalf("精修预留应退回：%+v", jv2.Money)
	}
	e.assertMoney(jv.JobID)
	// 失败后可以再精修，成功则换新视频
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r2", Note: "再来"}); err != nil {
		t.Fatalf("精修失败后应能再精修：%v", err)
	}
	jv3 := e.run(jv.JobID, 6)
	if jv3.Status != "succeeded" || jv3.Segments[1].VideoRid == rids[1] || jv3.Segments[1].Redos != 2 {
		t.Fatalf("再精修成功：%+v", jv3.Segments[1])
	}
	e.assertMoney(jv.JobID)
}

// 部分成功的任务上精修成功段：失败段不受影响，任务仍是部分成功；精修失败也不改状态。
func TestRefineOnPartialJobKeepsPartial(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("精修失败=%v", fail), func(t *testing.T) {
			e, _, _ := autoDeliveryEnv(t, nil)
			jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
			e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
			e.gw.script["seg/02/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
			jv = e.run(jv.JobID, 6)
			if jv.Status != "partial" {
				t.Fatalf("前置：%s", jv.Status)
			}
			if !jv.Segments[0].Refinable || jv.Segments[1].Refinable || !jv.Segments[1].Redoable {
				t.Fatalf("成功段可精修、失败段只能重做：%+v", jv.Segments)
			}
			if fail {
				e.gw.script["seg/01/generate#u1"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
			}
			if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1", Note: "亮一点"}); err != nil {
				t.Fatal(err)
			}
			jv = e.run(jv.JobID, 6)
			if jv.Status != "partial" || jv.SegDone != 2 || jv.SegFailed != 1 || jv.Segments[1].State != "failed" || jv.Result.Kind != ResultPreview {
				t.Fatalf("仍是部分成功、失败段仍是失败占位：%s %+v", jv.Status, jv.Result)
			}
			if jv.Segments[0].State != "succeeded" {
				t.Fatalf("%+v", jv.Segments[0])
			}
			e.assertMoney(jv.JobID)
		})
	}
}

func TestRefineLimitAndBusy(t *testing.T) {
	e, _, jv, _ := refineBase(t)
	for i := 1; i <= 3; i++ {
		if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: fmt.Sprintf("r%d", i), Note: "再改"}); err != nil {
			t.Fatalf("第 %d 次精修：%v", i, err)
		}
		if i == 1 { // 进行中不能叠加
			if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "busy", Note: "再改"}); reasonOf(err) != "SEGMENT_BUSY" {
				t.Fatalf("精修中再精修应 SEGMENT_BUSY：%v", err)
			}
		}
		e.run(jv.JobID, 6)
	}
	holds := e.ledger.holds
	jv2, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
	if jv2.Segments[1].Refinable || jv2.Segments[1].RefineLabel != "" || jv2.Segments[1].Redos != 3 || !jv2.Segments[0].Refinable {
		t.Fatalf("三次用完后这一段不再可精修，其它段不受影响：%+v", jv2.Segments)
	}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r4", Note: "再改"}); reasonOf(err) != "REDO_LIMIT" || e.ledger.holds != holds {
		t.Fatalf("第 4 次应 REDO_LIMIT 且不预留：%v", err)
	}
	e.assertMoney(jv.JobID)
}

func TestRefineIdempotentReplayAndQuoteChanged(t *testing.T) {
	e, _, jv, _ := refineBase(t)
	holds := e.ledger.holds
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: "动作大点", ExpectedPoints: 999}); reasonOf(err) != ReasonQuoteChanged || e.ledger.holds != holds {
		t.Fatalf("价变应 QUOTE_CHANGED 且不预留：%v", err)
	}
	if j, _ := e.store.GetJob(e.ctx, jv.JobID); j.Status != model.JobSucceeded || j.RedoCount != 0 {
		t.Fatalf("价变不得动任务 / 占名额：%d %d", j.Status, j.RedoCount)
	}
	for i := 0; i < 3; i++ { // 同键重复点击：只预留一次，再次推进完成后重放仍幂等
		if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: "动作大点", ExpectedPoints: jv.Segments[1].Points}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			e.run(jv.JobID, 6)
		}
	}
	if e.ledger.holds != holds+1 {
		t.Fatalf("同键只预留一次：%d → %d", holds, e.ledger.holds)
	}
	if j, _ := e.store.GetJob(e.ctx, jv.JobID); j.RedoCount != 1 {
		t.Fatalf("只占一次名额：%d", j.RedoCount)
	}
}

// 精修在途时「不做剩下的段」不动它；强制收尾也不让它拖成失败。
func TestRefineNotTouchedByCancelRest(t *testing.T) {
	e, _, jv, rids := refineBase(t)
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: "改"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.svc.JobView(e.ctx, uid, jv.JobID)
	if cur.CancelRest != nil && cur.CancelRest.Available {
		t.Fatalf("精修不是「剩下的段」：%+v", cur.CancelRest)
	}
	if _, err := e.svc.CancelRest(e.ctx, uid, jv.JobID); err != nil {
		t.Fatal(err)
	}
	jv2 := e.run(jv.JobID, 6)
	if jv2.Status != "succeeded" || jv2.Segments[1].State != "succeeded" || jv2.Segments[1].VideoRid == rids[1] {
		t.Fatalf("精修应照常做完：%s %+v", jv2.Status, jv2.Segments[1])
	}
}

func TestSegResult(t *testing.T) {
	mk := func(statuses ...string) []*model.Step {
		var out []*model.Step
		for i, s := range statuses {
			out = append(out, &model.Step{ID: int64(i + 1), Status: s})
		}
		return out
	}
	cases := []struct {
		name     string
		chain    []string
		wantIdx  int // 期望有效步骤下标，-1 = nil
		refining bool
	}{
		{"空链", nil, -1, false},
		{"只有成功", []string{model.StepSucceeded}, 0, false},
		{"原失败 + 重做失败", []string{model.StepFailed, model.StepFailed}, 1, false},
		{"失败后重做成功", []string{model.StepFailed, model.StepSucceeded}, 1, false},
		{"成功 + 精修排队", []string{model.StepSucceeded, model.StepPending}, 0, true},
		{"成功 + 精修出片中", []string{model.StepSucceeded, model.StepFailed, model.StepRunning}, 0, true},
		{"成功 + 精修待确认", []string{model.StepSucceeded, model.StepDelivering}, 0, true},
		{"成功 + 精修失败", []string{model.StepSucceeded, model.StepFailed}, 0, false},
		{"成功 + 精修被取消", []string{model.StepSucceeded, model.StepCanceled}, 0, false},
		{"成功 + 精修失败 + 精修成功", []string{model.StepSucceeded, model.StepFailed, model.StepSucceeded}, 2, false},
		{"两次成功 + 第三次失败取最近成功", []string{model.StepSucceeded, model.StepSucceeded, model.StepFailed}, 1, false},
		{"重做在途无成功", []string{model.StepFailed, model.StepRunning}, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chain := mk(c.chain...)
			eff, refining := segResult(chain)
			if refining != c.refining || (c.wantIdx < 0) != (eff == nil) || (eff != nil && eff != chain[c.wantIdx]) {
				t.Fatalf("eff=%v refining=%v，期望下标 %d refining=%v", eff, refining, c.wantIdx, c.refining)
			}
		})
	}
}
