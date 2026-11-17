package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"aigc-local-life/pkg/mediasvc"
)

const badOutputText = "画面有问题（黑屏 / 定格），这一段没收费，可以重做"

func TestBadOutputVerdict(t *testing.T) {
	ok := mediasvc.VideoInspect{Decodable: true, DurationS: 5}
	cases := []struct {
		name string
		in   mediasvc.VideoInspect
		bad  bool
	}{
		{"正常", ok, false},
		{"解不开", mediasvc.VideoInspect{Decodable: false}, true},
		{"黑屏刚好在阈值", mediasvc.VideoInspect{Decodable: true, BlackRatio: 0.6}, true},
		{"黑屏低于阈值", mediasvc.VideoInspect{Decodable: true, BlackRatio: 0.59}, false},
		{"定格刚好在阈值", mediasvc.VideoInspect{Decodable: true, FreezeRatio: 0.8}, true},
		{"定格低于阈值（片头静止镜头）", mediasvc.VideoInspect{Decodable: true, FreezeRatio: 0.7}, false},
	}
	for _, c := range cases {
		if bad, why := badOutputVerdict(c.in, 0.6, 0.8); bad != c.bad || (bad && why == "") {
			t.Errorf("%s: bad=%v why=%q", c.name, bad, why)
		}
	}
}

// 坏片：该段失败 BAD_OUTPUT、退回预留、不走 #r1 自动重试，文案固定，可付费重做。
func TestBadOutputFailsSegmentRefundsAndNoAutoRetry(t *testing.T) {
	e := newEnv(t, false)
	e.media.inspect = func(string) (mediasvc.VideoInspect, error) {
		return mediasvc.VideoInspect{Decodable: true, DurationS: 5, BlackRatio: 0.9}, nil
	}
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	jv = e.run(jv.JobID, 5)
	if jv.Status != "failed" || jv.Money.Refunded != 100 || jv.Money.Confirmed != 0 {
		t.Fatalf("坏片应失败并退回：%+v", jv)
	}
	sg := jv.Segments[0]
	if sg.State != "failed" || sg.Text != badOutputText || sg.MoneyText != "已退回 100 点" || !sg.Redoable {
		t.Fatalf("段文案 / 退点 / 可重做不对：%+v", sg)
	}
	if e.gw.submitted("#r1") != 0 {
		t.Fatal("BAD_OUTPUT 不得自动重试")
	}
	var code string
	for _, st := range e.store.steps {
		if st.JobID == jv.JobID {
			code = deref(st.ErrorCode)
		}
	}
	if code != FailBadOutput {
		t.Fatalf("错误码应为 BAD_OUTPUT：%q", code)
	}
	e.assertMoney(jv.JobID)
	// 用户付费重做：新预留、新步骤
	e.media.inspect = nil
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"}); err != nil {
		t.Fatalf("坏片后应可重做：%v", err)
	}
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 {
		t.Fatalf("重做成功只扣一次：%+v", jv.Money)
	}
	e.assertMoney(jv.JobID)
}

// 检测器自身故障 fail-open：按成功继续确认扣点；关闭开关则根本不检查。
func TestBadOutputCheckUnavailableFailsOpen(t *testing.T) {
	e := newEnv(t, false)
	e.media.inspect = func(string) (mediasvc.VideoInspect, error) {
		return mediasvc.VideoInspect{}, errors.New("媒体服务不可用")
	}
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID, 4)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 || jv.Money.Refunded != 0 {
		t.Fatalf("检测不可用应放行：%+v", jv)
	}
	if len(e.media.inspected) == 0 {
		t.Fatal("应调用过检测")
	}
	e.assertMoney(jv.JobID)

	off := newEnv(t, false)
	off.svc.cfg.BadOutputOff = true
	off.media.inspect = func(string) (mediasvc.VideoInspect, error) {
		return mediasvc.VideoInspect{Decodable: false}, nil
	}
	jv = off.run(off.confirm(off.plan(off.seedAnalysis(10)), "", "k1").JobID, 4)
	if jv.Status != "succeeded" || len(off.media.inspected) != 0 {
		t.Fatalf("关闭后不检查：%+v inspected=%v", jv.Status, off.media.inspected)
	}
}

// 没装配媒体服务：不检查、放行。
func TestBadOutputNoMediaPasses(t *testing.T) {
	e := newEnv(t, false)
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	e.svc.media = nil
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" {
		t.Fatalf("没媒体服务应放行：%+v", jv.Status)
	}
}

// 精修命中坏片：精修预留退回，这一段仍是已完成 + 旧视频，成片不变；不自动重试；仍可再精修。
func TestRefineBadOutputKeepsOldVideo(t *testing.T) {
	e, ops, jv, rids := refineBase(t)
	docBefore := resultDoc(t, e, jv.JobID)
	delsBefore := len(ops.dels)
	old := map[string]bool{}
	for _, r := range rids {
		old[r] = true
	}
	e.media.inspect = func(rid string) (mediasvc.VideoInspect, error) {
		if old[rid] {
			return mediasvc.VideoInspect{Decodable: true, DurationS: 5}, nil
		}
		return mediasvc.VideoInspect{Decodable: true, DurationS: 5, FreezeRatio: 1}, nil // 新产出的是定格片
	}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1", Note: "换个镜头"}); err != nil {
		t.Fatal(err)
	}
	jv2 := e.run(jv.JobID, 8)
	if e.gw.submitted("seg/02/generate#u1#r1") != 0 {
		t.Fatal("BAD_OUTPUT 的精修也不得自动重试")
	}
	if jv2.Status != "succeeded" || jv2.SegFailed != 0 || jv2.SegDone != 3 {
		t.Fatalf("精修坏片不得拖垮任务状态：%s done=%d failed=%d", jv2.Status, jv2.SegDone, jv2.SegFailed)
	}
	sg := jv2.Segments[1]
	if sg.State != "succeeded" || sg.VideoRid != rids[1] || sg.Redoable || !strings.Contains(sg.Text, "精修没成功") || sg.MoneyText != fmt.Sprintf("已退回 %d 点", sg.Points) {
		t.Fatalf("应仍是已完成 + 旧视频 + 精修失败说明 + 退点：%+v", sg)
	}
	if !sg.Refinable || sg.Redos != 1 {
		t.Fatalf("坏片的精修占一次名额，仍可再精修：%+v", sg)
	}
	docAfter := resultDoc(t, e, jv.JobID)
	if docAfter.Basis != docBefore.Basis || docAfter.VideoRid != docBefore.VideoRid || docAfter.Kind != ResultFull || len(docAfter.Missing) != 0 {
		t.Fatalf("成片应保持原样：%+v → %+v", docBefore, docAfter)
	}
	if len(ops.dels) != delsBefore {
		t.Fatalf("精修坏片不得新增入库：%d → %d", delsBefore, len(ops.dels))
	}
	if e.ledger.status(e.rRedo(jv.JobID, 2, 1)) != "REFUNDED" || jv2.Money.Confirmed != jv.Money.Confirmed {
		t.Fatalf("精修预留应退回、原扣费不变：%+v", jv2.Money)
	}
	e.assertMoney(jv.JobID)
	// 失败步骤本身记的是 BAD_OUTPUT 与固定文案（给运维 / 排障看；段视图沿用精修失败的统一说明）
	found := false
	for _, st := range e.store.steps {
		if st.JobID == jv.JobID && st.StepKey == "seg/02/generate#u1" {
			found = deref(st.ErrorCode) == FailBadOutput && deref(st.ErrorMsg) == badOutputText
		}
	}
	if !found {
		t.Fatal("精修步骤应记 BAD_OUTPUT 与固定文案")
	}
}
