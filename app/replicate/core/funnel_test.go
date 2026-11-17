package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/mediasvc"
)

// fakeFunnel 内存版漏斗事件表：dedupe_key 唯一；FunnelChannel 取同 job / 分析最早一条用户事件的渠道。
type fakeFunnel struct {
	mu   sync.Mutex
	rows []*model.FunnelEvent
	keys map[string]bool
	fail bool // 写库一律失败
}

func (f *fakeFunnel) InsertFunnelEvent(_ context.Context, e *model.FunnelEvent) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return false, errors.New("库故障")
	}
	if f.keys == nil {
		f.keys = map[string]bool{}
	}
	if f.keys[e.DedupeKey] {
		return false, nil
	}
	f.keys[e.DedupeKey] = true
	c := *e
	f.rows = append(f.rows, &c)
	return true, nil
}

func (f *fakeFunnel) FunnelChannel(_ context.Context, analysisID, jobID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Channel == ChannelSystem {
			continue
		}
		if (jobID > 0 && r.JobID == jobID) || (jobID == 0 && analysisID > 0 && r.AnalysisID == analysisID) {
			return r.Channel, nil
		}
	}
	return "", nil
}

// seq 事件序列：stage/channel。
func (f *fakeFunnel) seq() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.rows))
	for i, r := range f.rows {
		out[i] = r.Stage + "/" + r.Channel
	}
	return out
}

func (f *fakeFunnel) find(stage string) *model.FunnelEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Stage == stage {
			return r
		}
	}
	return nil
}

func (f *fakeFunnel) count(stage string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.Stage == stage {
			n++
		}
	}
	return n
}

func withFunnel(e *env) *fakeFunnel {
	ff := &fakeFunnel{}
	WithFunnel(ff)(e.svc)
	return ff
}

// 主链路：方案出卡 → 确认（先做第 1 段）→ 第 1 段做完 → 继续 → 成片交付；渠道取入口，系统推进的事件继承入口渠道；重放不重复记。
func TestFunnelStagesChannelsAndDedupe(t *testing.T) {
	e := newEnv(t, true)
	withComposer(e, "")
	ff := withFunnel(e)
	bg := e.ctx
	agent, web := WithChannel(bg, ChannelAgent), WithChannel(bg, ChannelWeb)

	e.ctx = agent
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.confirm(pv, "", "k1")
	e.ctx = bg // 轮询器没有入口渠道
	jv = e.run(jv.JobID, 3)
	if jv.Status != "waiting" {
		t.Fatalf("前置：应待继续：%s", jv.Status)
	}
	// 同一幂等键重放确认：不重复记
	e.ctx = agent
	e.confirm(pv, "", "k1")
	e.ctx = web
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1", ExpectedPoints: pv.Cost.Rest}); err != nil {
		t.Fatal(err)
	}
	e.ctx = bg
	e.run(jv.JobID, 6)

	want := []string{"plan_create/agent", "confirm_first/agent", "first_done/agent", "continue/web", "delivered/agent"}
	got := ff.seq()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("漏斗序列：\n got %v\nwant %v", got, want)
	}
	if pc := ff.find("plan_create"); pc.AnalysisID == 0 || pc.PlanID == 0 || pc.MultiPerson != 0 {
		t.Fatalf("方案事件归属 / 单人：%+v", pc)
	}
	if cf := ff.find("confirm_first"); cf.JobID == 0 || cf.PlanID != ff.find("plan_create").PlanID || cf.AnalysisID != ff.find("plan_create").AnalysisID {
		t.Fatalf("确认事件应带 job / plan / analysis：%+v", cf)
	}
	// 再推进（终态）不再产生事件
	e.run(jv.JobID, 3)
	if len(ff.seq()) != len(want) {
		t.Fatalf("终态重复推进不应再记事件：%v", ff.seq())
	}
}

// 没有任何渠道信息的事件记为 system；用户在待继续上选择不继续记 stop。
func TestFunnelStopAndSystemChannel(t *testing.T) {
	e := newEnv(t, true)
	ff := withFunnel(e)
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "", "k1") // 没设渠道：入口事件记 system
	jv = e.run(jv.JobID, 3)
	if _, err := e.svc.Stop(WithChannel(e.ctx, ChannelWeb), uid, jv.JobID); err != nil {
		t.Fatal(err)
	}
	want := []string{"plan_create/system", "confirm_first/system", "first_done/system", "stop/web"}
	if got := ff.seq(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("漏斗序列：\n got %v\nwant %v", got, want)
	}
}

// 多人原片：方案事件带 multi_person=1；分析成功事件同口径。
func TestFunnelMultiPerson(t *testing.T) {
	e := newEnv(t, false)
	ff := withFunnel(e)
	a := e.seedAnalysis(12.3)
	var r AnalysisResult
	_ = json.Unmarshal(a.Result, &r)
	r.Subjects = []Subject{{ID: "p1", Name: "甲"}, {ID: "p2", Name: "乙"}}
	a.Result, _ = json.Marshal(r)
	for _, x := range e.store.analyses { // 计划从库里读分析，要改库里那份
		if x.ID == a.ID {
			x.Result = a.Result
		}
	}
	e.plan(a)
	if pc := ff.find("plan_create"); pc == nil || pc.MultiPerson != 1 {
		t.Fatalf("多人原片应标 multi_person=1：%+v", pc)
	}
	// 分析成功：按写入的 result 判断
	set := map[string]any{"result": json.RawMessage(a.Result)}
	e.svc.funnelAnalysisDone(e.ctx, a, set)
	if as := ff.find("analysis_succeed"); as == nil || as.MultiPerson != 1 || as.AnalysisID != a.ID {
		t.Fatalf("分析成功事件：%+v", as)
	}
	if !funnelMulti(&AnalysisResult{Subjects: []Subject{{ID: "p1"}, {ID: "p2"}}}) || funnelMulti(&AnalysisResult{Subjects: []Subject{{ID: "p1"}}}) || funnelMulti(nil) {
		t.Fatal("主体数 >= 2 才算多人")
	}
}

// 定妆图 → 确认转化：同一个 plan_id 先有 preview 事件、后有 confirm 事件；定妆图回放不再记。
func TestFunnelPreviewThenConfirmSamePlan(t *testing.T) {
	pe, pv := newPreviewEnv(t)
	ff := withFunnel(pe.env)
	ctx := WithChannel(pe.ctx, ChannelAgent)
	if _, err := pe.svc.PreviewFrame(ctx, uid, pv.PlanNo); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.svc.PreviewFrame(ctx, uid, pv.PlanNo); err != nil { // 回放
		t.Fatal(err)
	}
	if ff.count("preview") != 1 {
		t.Fatalf("回放不应重复记定妆图事件：%v", ff.seq())
	}
	pe.env.ctx = ctx
	pe.confirm(pv, "", "k1")
	p, c := ff.find("preview"), ff.find("confirm_all")
	if c == nil {
		c = ff.find("confirm_first")
	}
	if p == nil || c == nil || p.PlanID == 0 || p.PlanID != c.PlanID || p.Channel != ChannelAgent {
		t.Fatalf("定妆图与确认应归同一方案：preview=%+v confirm=%+v", p, c)
	}
}

// 事件写库失败只告警，不影响业务调用；计数口径不受影响（调用照常成功）。
func TestFunnelWriteFailureNeverFailsBusiness(t *testing.T) {
	e := newEnv(t, false)
	ff := withFunnel(e)
	ff.fail = true
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" {
		t.Fatalf("漏斗写库失败不应影响出片：%s", jv.Status)
	}
	e.assertMoney(jv.JobID)
	if len(ff.seq()) != 0 {
		t.Fatalf("写库失败时不应有行：%v", ff.seq())
	}
}

// 坏片 / 重做 / 精修各记一次，且重做按幂等键去重。
func TestFunnelBadOutputRedo(t *testing.T) {
	e := newEnv(t, false)
	ff := withFunnel(e)
	e.media.inspect = func(string) (mediasvc.VideoInspect, error) {
		return mediasvc.VideoInspect{Decodable: true, DurationS: 5, BlackRatio: 0.9}, nil
	}
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	jv = e.run(jv.JobID, 5)
	if ff.count("bad_output") != 1 {
		t.Fatalf("应记 1 次坏片：%v", ff.seq())
	}
	e.media.inspect = nil
	ctx := WithChannel(e.ctx, ChannelAgent)
	for i := 0; i < 2; i++ { // 同键重复点击
		if _, err := e.svc.Redo(ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"}); err != nil {
			t.Fatal(err)
		}
	}
	if ff.count("redo") != 1 || ff.find("redo").SegIndex != 1 || ff.find("redo").Channel != ChannelAgent {
		t.Fatalf("重做应记 1 次（段 1、agent）：%v", ff.seq())
	}
}
