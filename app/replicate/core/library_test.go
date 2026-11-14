package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

// ---- 内存 LibraryStore：语义对齐 repo/library.go（锁原方案、单飞、按用户串行的上限） ----

type memLib struct {
	*memStore
	lmu     sync.Mutex
	samples []*model.Sample
	saved   []*model.SavedPlan
}

func (e *env) withLib() *memLib {
	l := &memLib{memStore: e.store}
	e.svc.lib = l
	return l
}

func (l *memLib) GetSample(_ context.Context, id int64) (*model.Sample, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	for _, s := range l.samples {
		if s.ID == id {
			return clone(s), nil
		}
	}
	return nil, nil
}

func (l *memLib) ListVisibleSamples(_ context.Context, industry string, limit int) ([]*model.Sample, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	var out []*model.Sample
	for _, s := range l.samples {
		if !s.Deleted && s.Enabled && (industry == "" || s.Industry == industry || s.Industry == "") {
			out = append(out, clone(s))
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].SortOrder < out[b].SortOrder })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (l *memLib) PromoteSampleAnalysis(_ context.Context, sampleID, pendingID int64) (bool, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	for _, s := range l.samples {
		if s.ID == sampleID && s.PendingAnalysisID != nil && *s.PendingAnalysisID == pendingID {
			s.AnalysisID, s.PendingAnalysisID, s.AnalysisError = &pendingID, nil, nil
			return true, nil
		}
	}
	return false, nil
}

func (l *memLib) FailSampleAnalysis(_ context.Context, sampleID, pendingID int64, reason string) (bool, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	for _, s := range l.samples {
		if s.ID == sampleID && s.PendingAnalysisID != nil && *s.PendingAnalysisID == pendingID {
			s.PendingAnalysisID, s.AnalysisError = nil, &reason
			return true, nil
		}
	}
	return false, nil
}

func (l *memLib) SupersedePlan(_ context.Context, parentID int64, child *model.Plan) (*model.Plan, bool, error) {
	m := l.memStore
	m.mu.Lock()
	defer m.mu.Unlock()
	var parent *model.Plan
	for _, p := range m.plans {
		if p.ID == parentID {
			parent = p
		}
	}
	if parent == nil {
		return nil, false, errors.New("no parent")
	}
	if parent.Status == model.PlanStale {
		for _, p := range m.plans {
			if parent.SupersededBy != nil && p.ID == *parent.SupersededBy && p.PlanHash == child.PlanHash && p.UserID == child.UserID {
				return clone(p), false, nil
			}
		}
		return nil, false, ErrPlanStale
	}
	if parent.Status != model.PlanReady {
		return nil, false, ErrPlanConfirming
	}
	var got *model.Plan
	created := false
	for _, p := range m.plans {
		if p.UserID == child.UserID && p.PlanHash == child.PlanHash {
			got = p
		}
	}
	if got == nil {
		got = clone(child)
		got.ID, got.CreatedAt = m.nextID(), m.now()
		m.plans = append(m.plans, got)
		created = true
	}
	if parent.Status == model.PlanReady {
		id := got.ID
		parent.Status, parent.SupersededBy = model.PlanStale, &id
	}
	return clone(got), created, nil
}

func (l *memLib) SavePlan(_ context.Context, sp *model.SavedPlan, max int, rename bool) (*model.SavedPlan, bool, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	n := 0
	for _, x := range l.saved {
		if x.UserID != sp.UserID {
			continue
		}
		if x.PlanID == sp.PlanID {
			if rename {
				x.Name = sp.Name
			}
			return clone(x), false, nil
		}
		n++
	}
	if n >= max {
		return nil, false, ErrSavedPlanLimit
	}
	c := clone(sp)
	c.ID = l.memStore.nextIDLocked()
	l.saved = append(l.saved, c)
	return clone(c), true, nil
}

func (m *memStore) nextIDLocked() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextID()
}

func (l *memLib) GetSavedPlan(_ context.Context, userID, id int64) (*model.SavedPlan, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	for _, x := range l.saved {
		if x.ID == id && x.UserID == userID {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (l *memLib) ListSavedPlans(_ context.Context, userID int64, offset, limit int) ([]*model.SavedPlan, int64, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	var all []*model.SavedPlan
	for i := len(l.saved) - 1; i >= 0; i-- {
		if l.saved[i].UserID == userID {
			all = append(all, clone(l.saved[i]))
		}
	}
	total := int64(len(all))
	if offset >= len(all) {
		return nil, total, nil
	}
	return all[offset:min(len(all), offset+limit)], total, nil
}

func (l *memLib) DeleteSavedPlan(_ context.Context, userID, id int64) (bool, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	for i, x := range l.saved {
		if x.ID == id && x.UserID == userID {
			l.saved = append(l.saved[:i], l.saved[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (l *memLib) TouchSavedPlan(_ context.Context, userID, id int64) error {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	for _, x := range l.saved {
		if x.ID == id && x.UserID == userID {
			x.UseCount++
		}
	}
	return nil
}

func (l *memLib) ChildPlans(_ context.Context, parentID int64) ([]*model.Plan, error) {
	l.memStore.mu.Lock()
	defer l.memStore.mu.Unlock()
	var out []*model.Plan
	for i := len(l.memStore.plans) - 1; i >= 0; i-- {
		if p := l.memStore.plans[i]; p.ParentID != nil && *p.ParentID == parentID {
			out = append(out, clone(p))
		}
	}
	return out, nil
}

func (l *memLib) ExistingAnalyses(_ context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, id := range ids {
		if a, _ := l.memStore.GetAnalysis(context.Background(), id); a != nil {
			out[id] = true
		}
	}
	return out, nil
}

// seedSample 一条已发布的示例（系统分析 user_id=0，时长 d）。
func (e *env) seedSample(l *memLib, industry string, d float64) *model.Sample {
	a := e.seedAnalysis(d)
	e.store.mu.Lock()
	for _, x := range e.store.analyses {
		if x.ID == a.ID {
			x.UserID, x.AnalysisNo, x.SourceKey = model.SystemUserID, "VRAsys"+fmt.Sprint(d), "sample-"+fmt.Sprint(d)
			x.SourceRid, x.SourceURL = ptr(testRid("smpsrc")), "mr://"+testRid("smpsrc")
		}
	}
	e.store.mu.Unlock()
	ann, _ := json.Marshal(Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}})
	s := &model.Sample{ID: int64(len(l.samples) + 1000), Title: "示例" + industry, Industry: industry, SourceRid: testRid("smpsrc"),
		CoverRid: ptr(testRid("smpcov")), ResultRid: ptr(testRid("smpres")), Annotations: ann, Enabled: true, AnalysisID: &a.ID}
	l.samples = append(l.samples, s)
	return s
}

func codeOf(err error) int {
	c, _, _ := xerr.Parse(err)
	return c
}

// 示例试看：只读，不建 job、不预留、不写方案；返回共享分析 + 示例原片 / 成片的签名地址。
func TestSampleTryIsFreeAndReadOnly(t *testing.T) {
	e := newEnv(t, false)
	l := e.withLib()
	smp := e.seedSample(l, "womenswear", 12.3)
	general := e.seedSample(l, "", 9)
	other := e.seedSample(l, "food", 8)
	off := e.seedSample(l, "womenswear", 7)
	off.Enabled = false

	list, err := e.svc.ListSamples(e.ctx, "womenswear")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, it := range list.Items {
		ids[it.ID] = true
		if it.CoverURL == "" || it.Summary == "" && it.DurationS == 0 {
			t.Fatalf("卡片应带封面签名与概述：%+v", it)
		}
	}
	if !ids[smp.ID] || !ids[general.ID] || ids[other.ID] || ids[off.ID] {
		t.Fatalf("按行业 + 通用、只列启用的：%v", ids)
	}
	plansBefore, holdsBefore := len(e.store.plans), e.ledger.holds
	for i := 0; i < 3; i++ {
		v, err := e.svc.TrySample(e.ctx, smp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Analysis == nil || v.Analysis.DurationS != 12.3 || v.SourceURL == "" || v.ResultURL == "" || v.ResultRid != testRid("smpres") ||
			v.Annotations == nil || v.Annotations.Dims["product"].Mode != ModeReplace || v.FreeText == "" {
			t.Fatalf("试看内容：%+v", v)
		}
	}
	if len(e.store.plans) != plansBefore || len(e.store.jobs) != 0 || e.ledger.holds != holdsBefore {
		t.Fatalf("试看不得建方案 / job / 预留：plans=%d jobs=%d holds=%d", len(e.store.plans), len(e.store.jobs), e.ledger.holds)
	}
	if _, err := e.svc.TrySample(e.ctx, off.ID); codeOf(err) != xerr.NotFound {
		t.Fatalf("停用的示例应找不到：%v", err)
	}
	// 系统分析不能被用户按编号拿到
	if _, err := e.svc.GetAnalysis(e.ctx, uid, "VRAsys12.3"); err == nil {
		t.Fatal("系统分析不应按用户编号可见")
	}
}

// 预分析惰性发布：进行中不替换已发布的；成功替换；失败只记原因。
func TestSyncSamplePromoteAndFail(t *testing.T) {
	e := newEnv(t, false)
	l := e.withLib()
	smp := e.seedSample(l, "", 12)
	oldID := *smp.AnalysisID
	pa := NewSampleAnalysis(smp.ID, 2, testRid("smpsrc"))
	got, _, _ := e.store.InsertAnalysis(e.ctx, pa)
	smp.PendingAnalysisID = &got.ID
	if s, _ := SyncSample(e.ctx, l, clone(smp)); *s.AnalysisID != oldID || s.PendingAnalysisID == nil {
		t.Fatalf("还在跑：不替换：%+v", s)
	}
	e.store.mu.Lock()
	for _, x := range e.store.analyses {
		if x.ID == got.ID {
			x.Status, x.Result = model.AnalysisSucceeded, []byte(`{"durationS":5}`)
		}
	}
	e.store.mu.Unlock()
	s, _ := SyncSample(e.ctx, l, clone(smp))
	if *s.AnalysisID != got.ID || s.PendingAnalysisID != nil {
		t.Fatalf("成功应发布：%+v", s)
	}
	// 再来一轮失败：已发布的不动，只记原因
	pa2, _, _ := e.store.InsertAnalysis(e.ctx, NewSampleAnalysis(smp.ID, 3, testRid("smpsrc")))
	l.samples[0].PendingAnalysisID = &pa2.ID
	e.store.mu.Lock()
	for _, x := range e.store.analyses {
		if x.ID == pa2.ID {
			x.Status, x.ErrorMsg = model.AnalysisFailed, ptr("原视频太短了")
		}
	}
	e.store.mu.Unlock()
	s, _ = SyncSample(e.ctx, l, clone(l.samples[0]))
	if *s.AnalysisID != got.ID || s.PendingAnalysisID != nil || deref(s.AnalysisError) != "原视频太短了" {
		t.Fatalf("失败只记原因：%+v", s)
	}
	if pa.UserID != model.SystemUserID || pa.SourceURL != "mr://"+testRid("smpsrc") || pa.SourceKey == pa2.SourceKey {
		t.Fatalf("系统分析行：%+v", pa)
	}
}

// 「换成我的素材」：用示例的共享分析出本人的方案，正常报价 / 确认 / 预留；原片是示例原片。
func TestPlanFromSampleChargesNormally(t *testing.T) {
	e := newEnv(t, false)
	l := e.withLib()
	smp := e.seedSample(l, "", 12.3)
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, SampleID: smp.ID, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if pv.SampleID != smp.ID || pv.Cost.Total != 130 || pv.Annotations == nil || len(pv.RefImages) != 1 {
		t.Fatalf("示例方案：%+v", pv)
	}
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if p.UserID != uid || p.AnalysisID != *smp.AnalysisID || p.SampleID == nil || *p.SampleID != smp.ID {
		t.Fatalf("方案行属于用户、引用示例分析：%+v", p)
	}
	jv := e.confirm(pv, "", "k-smp")
	j, _ := e.store.GetJob(e.ctx, jv.JobID)
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	if in.SourceRid != testRid("smpsrc") || e.ledger.holds != 1 {
		t.Fatalf("出片读示例原片、正常预留：%+v holds=%d", in, e.ledger.holds)
	}
	// 别人看不到这张方案
	if _, err := e.svc.GetPlan(e.ctx, uid+1, pv.PlanNo); codeOf(err) != xerr.NotFound {
		t.Fatalf("越权取方案：%v", err)
	}
	// 停用的示例不能再出方案
	l.samples[0].Enabled = false
	if _, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, SampleID: smp.ID, ModelGroup: "vid.seedance-2.5"}); codeOf(err) != xerr.NotFound {
		t.Fatalf("停用示例：%v", err)
	}
}

// 改标注：新方案（新指纹 / 报价），旧方案 STALE 不可确认；同一改动重放拿回同一张；在旧方案上的不同改动 409。
func TestPatchAnnotationsSupersedes(t *testing.T) {
	e := newEnv(t, false)
	e.withLib()
	a := e.seedAnalysis(12.3)
	pv := e.plan(a)
	describe := Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "scene": {Mode: ModeDescribe, Note: "换成海边"}}}
	nv, created, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: describe})
	if err != nil || !created {
		t.Fatalf("改标注：%v %v", created, err)
	}
	if nv.PlanNo == pv.PlanNo || nv.PlanHash == pv.PlanHash || nv.ParentPlanNo != pv.PlanNo || len(nv.RefImages) != 1 || nv.Status != model.PlanReady {
		t.Fatalf("新方案：%+v", nv)
	}
	old, _ := e.svc.GetPlan(e.ctx, uid, pv.PlanNo)
	if old.Status != model.PlanStale || old.SupersededBy != nv.PlanNo {
		t.Fatalf("旧方案应 STALE：%+v", old)
	}
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k-old"}); reasonOf(err) != ReasonPlanStale {
		t.Fatalf("旧方案确认应 409 PLAN_STALE：%v", err)
	}
	if len(e.store.jobs) != 0 || e.ledger.holds != 0 {
		t.Fatal("PLAN_STALE 不建 job、不预留")
	}
	// 重放
	rv, created, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: describe})
	if err != nil || created || rv.PlanNo != nv.PlanNo {
		t.Fatalf("同一改动重放应拿回同一张：%v %v %+v", created, err, rv)
	}
	// 在旧方案上做不同的改动
	if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo,
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "light": {Mode: ModeDescribe, Note: "更亮"}}}}); reasonOf(err) != ReasonPlanStale {
		t.Fatalf("旧方案上的不同改动应 409：%v", err)
	}
	// 改回最初的标注：是一张新方案（指纹带来源），不复活旧方案
	back, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: nv.PlanNo,
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil || back.PlanHash == pv.PlanHash || back.PlanNo == pv.PlanNo {
		t.Fatalf("改回旧标注也是新方案：%v %+v", err, back)
	}
	// 新方案能正常确认
	jv := e.confirm(back, "", "k-new")
	if jv.JobID == 0 || e.ledger.holds != 1 {
		t.Fatalf("新方案确认：%+v", jv)
	}
	// 已确认的方案再改：一律 409 PLAN_CONFIRMING，不出子方案
	n := len(e.store.plans)
	if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: back.PlanNo, Annotations: describe}); reasonOf(err) != ReasonPlanConfirming {
		t.Fatalf("已确认方案再改应 409 PLAN_CONFIRMING：%v", err)
	} else if _, msg, _ := xerr.Parse(err); msg != planConfirmingText {
		t.Fatalf("文案：%q", msg)
	}
	if len(e.store.plans) != n {
		t.Fatal("PLAN_CONFIRMING 不得插入子方案")
	}
	if cur, _ := e.svc.GetPlan(e.ctx, uid, back.PlanNo); cur.Status != model.PlanConfirmed {
		t.Fatalf("已确认方案不动：%s", cur.Status)
	}
	cv := e.plan(e.seedAnalysis(10))
	// 缺项：原方案不动
	_, _, err = e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: cv.PlanNo, Annotations: Annotations{Dims: map[string]DimEdit{"scene": {Mode: ModeDescribe}}}})
	var ge *GapsError
	if !errors.As(err, &ge) {
		t.Fatalf("缺项应 gaps：%v", err)
	}
	if cur, _ := e.svc.GetPlan(e.ctx, uid, cv.PlanNo); cur.Status != model.PlanReady {
		t.Fatal("缺项不动原方案")
	}
	// 越权
	if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid + 1, PlanNo: cv.PlanNo, Annotations: describe}); codeOf(err) != xerr.NotFound {
		t.Fatalf("改别人的方案应找不到：%v", err)
	}
	// 换图：传了 refImages 就整体替换
	nr := []string{"mr://" + testRid("refb"), "mr://" + testRid("refc")}
	rv2, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: cv.PlanNo, RefImages: &nr,
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{2}}}}})
	if err != nil || len(rv2.RefImages) != 2 || rv2.RefImages[1].Rid != testRid("refc") {
		t.Fatalf("换图：%v %+v", err, rv2)
	}
}

// 我的方案：保存（同一方案只存一份）、列表、再做一条（新指纹、同键重放）、删除、原片不在、上限、越权。
func TestSavedPlans(t *testing.T) {
	e := newEnv(t, false)
	l := e.withLib()
	a := e.seedAnalysis(12.3)
	pv := e.plan(a)
	sv, err := e.svc.SavePlan(e.ctx, uid, pv.PlanNo, "")
	if err != nil || !sv.Created || sv.Name != "白色运动鞋·换商品" || len(sv.Labels) != 1 || !sv.SourceAvailable {
		t.Fatalf("保存：%v %+v", err, sv)
	}
	again, err := e.svc.SavePlan(e.ctx, uid, pv.PlanNo, "夏季爆款")
	if err != nil || again.Created || again.ID != sv.ID || again.Name != "夏季爆款" {
		t.Fatalf("再存只改名：%v %+v", err, again)
	}
	if _, err := e.svc.SavePlan(e.ctx, uid+1, pv.PlanNo, ""); codeOf(err) != xerr.NotFound {
		t.Fatalf("存别人的方案应找不到：%v", err)
	}
	list, _ := e.svc.ListSavedPlans(e.ctx, uid, 1, 20)
	if list.Total != 1 || list.Limit != 50 || list.Items[0].RefImages[0].URL == "" {
		t.Fatalf("列表：%+v", list)
	}
	if other, _ := e.svc.ListSavedPlans(e.ctx, uid+1, 1, 20); other.Total != 0 {
		t.Fatal("别人看不到")
	}
	// 再做一条：默认原片 + 原图，新指纹
	rv, created, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, IdempotencyKey: "r1"})
	if err != nil || !created || rv.PlanNo == pv.PlanNo || rv.PlanHash == pv.PlanHash || len(rv.RefImages) != 1 {
		t.Fatalf("再做一条：%v %v %+v", created, err, rv)
	}
	if rv2, created, _ := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, IdempotencyKey: "r1"}); created || rv2.PlanNo != rv.PlanNo {
		t.Fatal("同键重放拿回同一张")
	}
	if rv3, _, _ := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, IdempotencyKey: "r2"}); rv3.PlanNo == rv.PlanNo {
		t.Fatal("新的一次是新方案")
	}
	if l.saved[0].UseCount != 2 {
		t.Fatalf("使用次数只记新建：%d", l.saved[0].UseCount)
	}
	// 换一条新视频（本人的分析），换图
	b := e.seedAnalysis(9)
	nr := []string{"mr://" + testRid("newp")}
	nv, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, AnalysisNo: b.AnalysisNo, RefImages: &nr})
	if err != nil || nv.Analysis.DurationS != 9 || nv.RefImages[0].Rid != testRid("newp") {
		t.Fatalf("新视频 + 新图：%v %+v", err, nv)
	}
	// 越权
	if _, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid + 1, SavedID: sv.ID}); codeOf(err) != xerr.NotFound {
		t.Fatalf("用别人的方案应找不到：%v", err)
	}
	if err := e.svc.DeleteSavedPlan(e.ctx, uid+1, sv.ID); codeOf(err) != xerr.NotFound {
		t.Fatalf("删别人的方案应找不到：%v", err)
	}
	// 原片不在了
	e.store.mu.Lock()
	for i, x := range e.store.analyses {
		if x.ID == a.ID {
			e.store.analyses = append(e.store.analyses[:i], e.store.analyses[i+1:]...)
			break
		}
	}
	e.store.mu.Unlock()
	if list, _ := e.svc.ListSavedPlans(e.ctx, uid, 1, 20); list.Items[0].SourceAvailable {
		t.Fatal("原片不在应标出")
	}
	if _, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID}); reasonOf(err) != ReasonSourceGone {
		t.Fatalf("原片不在：%v", err)
	}
	if err := e.svc.DeleteSavedPlan(e.ctx, uid, sv.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.ListSavedPlans(e.ctx, uid, 1, 20); list.Total != 0 {
		t.Fatal("删除后为空")
	}
	// 上限
	for i := 0; i < model.MaxSavedPlans; i++ {
		l.saved = append(l.saved, &model.SavedPlan{ID: int64(90000 + i), UserID: uid, PlanID: int64(80000 + i), Annotations: []byte(`{}`)})
	}
	if _, err := e.svc.SavePlan(e.ctx, uid, rv.PlanNo, ""); reasonOf(err) != ReasonSavedPlanLimit {
		t.Fatalf("满 50 应 409：%v", err)
	}
}

// 用示例出的方案保存后再做：默认沿用示例分析；示例分析不能被别人的保存借道。
func TestSavedPlanFromSample(t *testing.T) {
	e := newEnv(t, false)
	l := e.withLib()
	smp := e.seedSample(l, "", 12.3)
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, SampleID: smp.ID, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	sv, err := e.svc.SavePlan(e.ctx, uid, pv.PlanNo, "")
	if err != nil || sv.SampleID != smp.ID {
		t.Fatalf("%v %+v", err, sv)
	}
	rv, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, IdempotencyKey: "x"})
	if err != nil || rv.SampleID != smp.ID {
		t.Fatalf("沿用示例分析：%v %+v", err, rv)
	}
	// 伪造：别人的保存行指向系统分析但不是示例来源 → 原片不在
	l.saved = append(l.saved, &model.SavedPlan{ID: 777, UserID: uid + 1, AnalysisID: *smp.AnalysisID, Annotations: []byte(`{"dims":{"product":{"mode":"replace","refs":[1]}}}`),
		ModelGroup: "vid.seedance-2.5", Resolution: "720p"})
	if _, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid + 1, SavedID: 777}); reasonOf(err) != ReasonSourceGone {
		t.Fatalf("不得借道系统分析：%v", err)
	}
}

func TestValidSampleAnnotationsAndRid(t *testing.T) {
	if a, err := ValidSampleAnnotations(nil); a != nil || err != nil {
		t.Fatal("空 = 没有改法")
	}
	if _, err := ValidSampleAnnotations([]byte(`{"dims":{"bogus":{"mode":"keep"}}}`)); err == nil {
		t.Fatal("未知项应拒")
	}
	if _, err := ValidSampleAnnotations([]byte(`{"dims":{"product":{"mode":"swap"}}}`)); err == nil {
		t.Fatal("未知改法应拒")
	}
	if _, err := ValidSampleAnnotations([]byte(`[1]`)); err == nil {
		t.Fatal("非对象应拒")
	}
	asset := "abc123-asset-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	if r, why := SampleRidOf("mr://" + asset); why != "" || r != asset || !mediasvc.ValidRID(r) {
		t.Fatal("mr:// 规范化")
	}
	if _, why := SampleRidOf("https://x/y.mp4"); why == "" {
		t.Fatal("地址不是 rid")
	}
	if _, why := SampleRidOf(testRid("x")); why == "" || !strings.Contains(why, "用户上传") {
		t.Fatalf("用户上传类 rid 拒收：%q", why)
	}
	if _, why := SampleRidOf("abc123-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"); why == "" {
		t.Fatal("中间产物类 rid 拒收")
	}
}

// 改标注时把原方案里的图原样传回（mr://rid）：即使它不是上传类别（http 拉入的）也认；没见过的非上传 rid 仍拒收。
func TestPatchAcceptsKnownRefRids(t *testing.T) {
	e := newEnv(t, false)
	e.withLib()
	a := e.seedAnalysis(12.3)
	pv := e.plan(a)
	ingested := "abc123-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	e.store.mu.Lock()
	for _, p := range e.store.plans {
		if p.PlanNo == pv.PlanNo {
			p.Refs = []byte(`[{"index":1,"rid":"` + ingested + `"}]`)
		}
	}
	e.store.mu.Unlock()
	ann := Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "light": {Mode: ModeDescribe, Note: "暖一点"}}}
	refs := []string{"mr://" + ingested}
	if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann, RefImages: &refs}); err != nil {
		t.Fatalf("原方案里的图应认：%v", err)
	}
	pv2 := e.plan(e.seedAnalysis(9))
	other := []string{"mr://abc123-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"}
	_, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv2.PlanNo, Annotations: ann, RefImages: &other})
	var ge *GapsError
	if !errors.As(err, &ge) || ge.Gaps[0].Code != "REF_NOT_OWNED" {
		t.Fatalf("没见过的非上传 rid 应拒收：%v", err)
	}
}

// 示例下架 / 换原片 / 重新分析后：基于示例的保存方案「再做一条」与示例方案的改标注都回 404 SOURCE_GONE；已确认的 job 不受影响。
func TestSampleOfflineBlocksReuseAndPatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(s *model.Sample, e *env)
	}{
		{"停用", func(s *model.Sample, _ *env) { s.Enabled = false }},
		{"删除", func(s *model.Sample, _ *env) { s.Deleted = true }},
		{"换原片清掉分析", func(s *model.Sample, _ *env) { s.AnalysisID = nil }},
		{"重新分析发布了新的一行", func(s *model.Sample, e *env) {
			b := e.seedAnalysis(20)
			e.store.mu.Lock()
			for _, x := range e.store.analyses {
				if x.ID == b.ID {
					x.UserID = model.SystemUserID
				}
			}
			e.store.mu.Unlock()
			s.AnalysisID = &b.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, false)
			l := e.withLib()
			smp := e.seedSample(l, "", 12.3)
			ann := Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}
			pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, SampleID: smp.ID, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef}, Annotations: ann})
			if err != nil {
				t.Fatal(err)
			}
			sv, err := e.svc.SavePlan(e.ctx, uid, pv.PlanNo, "")
			if err != nil {
				t.Fatal(err)
			}
			done := e.plan(e.seedAnalysis(9)) // 不相关的本人方案：照常
			_ = done
			jpv, _ := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, SampleID: smp.ID, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
				Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "light": {Mode: ModeDescribe, Note: "亮"}}}})
			jv := e.confirm(jpv, "", "k-before-offline")

			tc.mut(l.samples[0], e)

			if _, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, IdempotencyKey: "r"}); reasonOf(err) != ReasonSourceGone || err.Error() == "" {
				t.Fatalf("再做一条应 SOURCE_GONE：%v", err)
			} else if _, msg, _ := xerr.Parse(err); msg != sampleOffText {
				t.Fatalf("文案：%q", msg)
			}
			if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo,
				Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "scene": {Mode: ModeDescribe, Note: "海边"}}}}); reasonOf(err) != ReasonSourceGone {
				t.Fatalf("改标注应 SOURCE_GONE：%v", err)
			}
			if cur, _ := e.svc.GetPlan(e.ctx, uid, pv.PlanNo); cur.Status != model.PlanReady {
				t.Fatal("拒收不动原方案")
			}
			// 已确认的 job 照常推进（输入已冻结）
			if v, err := e.svc.JobView(e.ctx, uid, jv.JobID); err != nil || v == nil {
				t.Fatalf("在途 job 不受影响：%v", err)
			}
			// 换成本人的视频再做：照常
			b := e.seedAnalysis(10)
			if _, _, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, AnalysisNo: b.AnalysisNo}); err != nil {
				t.Fatalf("换成自己的视频照常：%v", err)
			}
		})
	}
}

type countLedger struct {
	*fakeLedger
	quotes int
}

func (c *countLedger) Quote(ctx context.Context, ch billing.Charge) (int64, error) {
	c.quotes++
	return c.fakeLedger.Quote(ctx, ch)
}

// 改标注重放：命中已有子方案直接返回，不再报价 / 解析参考图（不打下游）；已 STALE 且不同改动直接 409，也不报价。
func TestPatchReplaySkipsDownstream(t *testing.T) {
	e := newEnv(t, false)
	e.withLib()
	cl := &countLedger{fakeLedger: e.ledger}
	e.svc.ledger = cl
	a := e.seedAnalysis(12.3)
	pv := e.plan(a)
	ann := Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "scene": {Mode: ModeDescribe, Note: " 换成海边 "}}}
	nv, created, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann})
	if err != nil || !created {
		t.Fatal(err)
	}
	before := cl.quotes
	for i := 0; i < 3; i++ {
		rv, created, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann})
		if err != nil || created || rv.PlanNo != nv.PlanNo || rv.Cost.Total != nv.Cost.Total {
			t.Fatalf("重放应命中子方案：%v %v %+v", created, err, rv)
		}
	}
	// 显式传回同样的图（mr://rid）也算同一改动
	same := []string{testRef}
	if rv, created, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann, RefImages: &same}); err != nil || created || rv.PlanNo != nv.PlanNo {
		t.Fatalf("同图重放：%v %v", created, err)
	}
	// 不同改动 + http 图：直接 409，不拉图、不报价
	ingests := e.media.ingests
	httpRef := []string{"https://own.test/aigc/image/7/20261113/p.png"}
	if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann, RefImages: &httpRef}); reasonOf(err) != ReasonPlanStale {
		t.Fatalf("应 409：%v", err)
	}
	if cl.quotes != before || e.media.ingests != ingests {
		t.Fatalf("重放 / 409 不得打下游：quotes %d→%d ingests %d→%d", before, cl.quotes, ingests, e.media.ingests)
	}
}

// 确认占用方案期间改标注：409 PLAN_CONFIRMING，不出子方案；确认失败放回后原方案回 READY、可以照常改。
// 旧数据（占用期间已有子方案）：放回时原方案 STALE 指向最新子方案，READY 子方案只留最新一张。
func TestPatchWhileConfirmingAndReleaseConverges(t *testing.T) {
	e := newEnv(t, false)
	e.withLib()
	a := e.seedAnalysis(12.3)
	pv := e.plan(a)
	ann := Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "light": {Mode: ModeDescribe, Note: "亮"}}}
	hooked := false
	e.ledger.balance = 0 // 预留会失败 → 放回方案
	e.ledger.onHold = func(string) {
		if hooked {
			return
		}
		hooked = true
		if _, _, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann}); reasonOf(err) != ReasonPlanConfirming {
			t.Errorf("占用期间改标注应 409 PLAN_CONFIRMING：%v", err)
		}
	}
	n := len(e.store.plans)
	if _, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"}); err == nil || !hooked {
		t.Fatalf("余额不足应失败、钩子应触发：%v %v", err, hooked)
	}
	if len(e.store.plans) != n {
		t.Fatal("占用期间不得插入子方案")
	}
	if cur, _ := e.svc.GetPlan(e.ctx, uid, pv.PlanNo); cur.Status != model.PlanReady {
		t.Fatalf("没有子方案放回 READY：%s", cur.Status)
	}
	e.ledger.onHold, e.ledger.balance = nil, -1
	if _, created, err := e.svc.PatchAnnotations(e.ctx, PatchReq{UserID: uid, PlanNo: pv.PlanNo, Annotations: ann}); err != nil || !created {
		t.Fatalf("放回后照常改：%v %v", created, err)
	}

	// 旧数据收敛：直接构造占用中的原方案 + 两张 READY 子方案
	p2 := e.plan(e.seedAnalysis(9))
	parent, _ := e.store.GetPlanByNo(e.ctx, uid, p2.PlanNo)
	if ok, _ := e.store.ConfirmPlan(e.ctx, parent.ID, 999); !ok {
		t.Fatal("占用")
	}
	var kids []*model.Plan
	for i := 0; i < 2; i++ {
		c, _, _ := e.store.InsertPlan(e.ctx, &model.Plan{PlanNo: fmt.Sprintf("VRPkid%d", i), UserID: uid, AnalysisID: parent.AnalysisID, ParentID: &parent.ID,
			Status: model.PlanReady, PlanHash: fmt.Sprintf("kid%d", i)})
		kids = append(kids, c)
	}
	if ok, _ := e.store.ReleasePlan(e.ctx, parent.ID, 999); !ok {
		t.Fatal("放回")
	}
	cur, _ := e.store.GetPlan(e.ctx, parent.ID)
	old, _ := e.store.GetPlan(e.ctx, kids[0].ID)
	newest, _ := e.store.GetPlan(e.ctx, kids[1].ID)
	if cur.Status != model.PlanStale || *cur.SupersededBy != newest.ID || old.Status != model.PlanStale || *old.SupersededBy != newest.ID || newest.Status != model.PlanReady {
		t.Fatalf("收敛：parent=%s old=%s newest=%s", cur.Status, old.Status, newest.Status)
	}
}

// 「再做一条」没带幂等键：按保存 id + 素材派生，重复点只出一张；换了素材才是新的一张。
func TestReuseDerivedKey(t *testing.T) {
	e := newEnv(t, false)
	e.withLib()
	pv := e.plan(e.seedAnalysis(12.3))
	sv, _ := e.svc.SavePlan(e.ctx, uid, pv.PlanNo, "")
	r1, c1, err := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID})
	r2, c2, _ := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID})
	if err != nil || !c1 || c2 || r1.PlanNo != r2.PlanNo {
		t.Fatalf("同素材不带键应同一张：%v %v %v", c1, c2, err)
	}
	nr := []string{"mr://" + testRid("other")}
	r3, c3, _ := e.svc.ReuseSavedPlan(e.ctx, ReuseReq{UserID: uid, SavedID: sv.ID, RefImages: &nr})
	if !c3 || r3.PlanNo == r1.PlanNo {
		t.Fatal("换素材是新的一张")
	}
}
