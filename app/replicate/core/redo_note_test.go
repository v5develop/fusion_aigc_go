package core

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/aigateway"
)

func TestNormalizeRedoNote(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"空", "", "", false},
		{"只有空白", " \n\t ", "", false},
		{"去首尾空白", "  动作再大一点  ", "动作再大一点", false},
		{"换行压成空格", "动作大一点\n\n镜头慢一点", "动作大一点 镜头慢一点", false},
		{"恰好 200 字", strings.Repeat("字", 200), strings.Repeat("字", 200), false},
		{"201 字拒收", strings.Repeat("字", 201), "", true},
		{"按字数不按字节", strings.Repeat("a", 200), strings.Repeat("a", 200), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeRedoNote(c.in)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got=%q err=%v 期望=%q err=%v", got, err, c.want, c.wantErr)
			}
			if err != nil && reasonOf(err) != "PARAM_INVALID" && !strings.Contains(err.Error(), "200") {
				t.Fatalf("超长应是参数错误并说清上限：%v", err)
			}
		})
	}
}

func promptOf(t *testing.T, e *env, suffix string) string {
	t.Helper()
	for k, r := range e.gw.reqs {
		if strings.HasSuffix(k, suffix) {
			return fmt.Sprint(r.Input.(map[string]any)["prompt"])
		}
	}
	t.Fatalf("没有提交过 %s：%v", suffix, e.gw.submits)
	return ""
}

// 备注只进这一次重做步骤（及它的自动重试）的提示词；原步骤、其它请求不变；job.input 不被改写。
func TestRedoNoteOnlyInRedoStepAndItsRetry(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	jv = e.run(jv.JobID, 5)
	if jv.Status != "failed" {
		t.Fatalf("前置：应失败 %+v", jv)
	}
	base := promptOf(t, e, "seg/01/generate")
	jobBefore, _ := e.store.GetJob(e.ctx, jv.JobID)

	// #u1 先失败一次（暂时性）→ 自动重试 #u1#r1 成功
	e.gw.script["seg/01/generate#u1"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	jv, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1", Note: "  动作再大一点\n"})
	if err != nil {
		t.Fatalf("带备注重做应成功：%v", err)
	}
	jv = e.run(jv.JobID, 5)
	if jv.Status != "succeeded" {
		t.Fatalf("重做应成功：%+v", jv)
	}
	want := base + "\n补充要求：动作再大一点"
	if got := promptOf(t, e, "seg/01/generate#u1"); got != want {
		t.Fatalf("重做步骤提示词：\n got=%q\nwant=%q", got, want)
	}
	if got := promptOf(t, e, "seg/01/generate#u1#r1"); got != want {
		t.Fatalf("重做的自动重试必须沿用备注：%q", got)
	}
	for _, k := range []string{"seg/01/generate#r1"} {
		if got := promptOf(t, e, k); got != base {
			t.Fatalf("原步骤 / 它的重试不得带备注 %s：%q", k, got)
		}
	}
	jobAfter, _ := e.store.GetJob(e.ctx, jv.JobID)
	if string(jobAfter.Input) != string(jobBefore.Input) {
		t.Fatal("备注不得写进冻结输入 job.input")
	}
	e.assertMoney(jv.JobID)
}

// 不带备注：请求与原来逐字节一致（回归）。
func TestRedoWithoutNoteUnchanged(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	jv = e.run(jv.JobID, 5)
	base := promptOf(t, e, "seg/01/generate")
	jv, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1", Note: "   "})
	if err != nil {
		t.Fatal(err)
	}
	e.run(jv.JobID, 5)
	if got := promptOf(t, e, "seg/01/generate#u1"); got != base {
		t.Fatalf("空备注不应改提示词：%q", got)
	}
}

func TestRedoNoteTooLongRejectedBeforeMoney(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	jv = e.run(jv.JobID, 5)
	holds := e.ledger.holds
	_, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1", Note: strings.Repeat("长", 201)})
	if err == nil || e.ledger.holds != holds {
		t.Fatalf("超长备注应拒收且不预留：%v holds=%d→%d", err, holds, e.ledger.holds)
	}
}

// 备注审核：不通过 → MODERATION_REJECTED，不占名额、不预留；通过 → 正常重做。
func TestRedoNoteModeration(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("pass")
	e.gw.analysis[opModerationText] = verdicts("pass")
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "DEADLINE_EXCEEDED"}}
	jv = e.run(jv.JobID, 5)
	holds := e.ledger.holds
	redoBefore, _ := e.store.GetJob(e.ctx, jv.JobID)

	e.gw.analysis[opModerationText] = verdicts("block")
	_, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1", Note: "违规内容"})
	if reasonOf(err) != ReasonModerationRejected || strings.Contains(err.Error(), "porn") {
		t.Fatalf("应 MODERATION_REJECTED 且不暴露标签：%v", err)
	}
	redoAfter, _ := e.store.GetJob(e.ctx, jv.JobID)
	if e.ledger.holds != holds || redoAfter.RedoCount != redoBefore.RedoCount {
		t.Fatalf("审核不过不得预留 / 占名额：holds=%d→%d redo=%d→%d", holds, e.ledger.holds, redoBefore.RedoCount, redoAfter.RedoCount)
	}
	// 同一备注、新请求键：通过后重做成功，备注进提示词
	e.gw.analysis[opModerationText] = verdicts("pass")
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r2", Note: "动作再大一点"}); err != nil {
		t.Fatalf("通过后应重做成功：%v", err)
	}
	e.run(jv.JobID, 5)
	if got := promptOf(t, e, "seg/01/generate#u1"); !strings.HasSuffix(got, "\n补充要求：动作再大一点") {
		t.Fatalf("通过的备注应进提示词：%q", got)
	}
}
