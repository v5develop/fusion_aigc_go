package repo

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"

	"github.com/uptrace/bun"
)

// 集成（REPLICATE_TEST_DSN，已执行 Go_V1.0.37 + 39 + 41 的临时库）：改标注取代、我的方案并发上限、示例预分析。

func libClean(t *testing.T, db *bun.DB) {
	clean := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_saved_plan WHERE user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_user_pref WHERE user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE a FROM afv_replicate_analysis a JOIN afv_replicate_sample s ON a.id IN (s.analysis_id, s.pending_analysis_id) WHERE s.created_by = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_analysis WHERE user_id = 0 AND source_url LIKE 'mr://itlib-%'")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_sample WHERE created_by = 990077")
	}
	clean()
	t.Cleanup(clean)
}

func insPlan(t *testing.T, r *Repo, hash, status string) *model.Plan {
	t.Helper()
	p := &model.Plan{PlanNo: "VRPit" + hash, UserID: 990077, AnalysisID: 1, Status: status, ModelGroup: "vid.x", Resolution: "720p",
		Annotations: []byte(`{}`), Segments: []byte(`[]`), Prompts: []byte(`[]`), PlanHash: hash, ExpiresAt: time.Now().Add(time.Hour)}
	got, created, err := r.InsertPlan(ctx, p)
	if err != nil || !created {
		t.Fatalf("插方案：%v %v", created, err)
	}
	return got
}

func childPlan(parent *model.Plan, hash string) *model.Plan {
	return &model.Plan{PlanNo: "VRPit" + hash, UserID: 990077, AnalysisID: 1, ParentID: &parent.ID, Status: model.PlanReady, ModelGroup: "vid.x",
		Resolution: "720p", Annotations: []byte(`{"dims":{}}`), Segments: []byte(`[]`), Prompts: []byte(`[]`), PlanHash: hash, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestIntegrationSupersedePlan(t *testing.T) {
	r, _ := integrationRepo(t)
	parent := insPlan(t, r, "itparent", model.PlanReady)
	got, created, err := r.SupersedePlan(ctx, parent.ID, childPlan(parent, "itchild1"))
	if err != nil || !created || got.ID == 0 || got.ExpiresAt.IsZero() {
		t.Fatalf("改标注：%+v %v %v", got, created, err)
	}
	cur, _ := r.GetPlan(ctx, parent.ID)
	if cur.Status != model.PlanStale || cur.SupersededBy == nil || *cur.SupersededBy != got.ID {
		t.Fatalf("原方案应 STALE + superseded_by：%+v", cur)
	}
	if ok, _ := r.ConfirmPlan(ctx, parent.ID, 1); ok {
		t.Fatal("STALE 方案的确认 CAS 不能成功")
	}
	again, created, err := r.SupersedePlan(ctx, parent.ID, childPlan(parent, "itchild1"))
	if err != nil || created || again.ID != got.ID {
		t.Fatalf("同一改动重放：%+v %v %v", again, created, err)
	}
	if _, _, err := r.SupersedePlan(ctx, parent.ID, childPlan(parent, "itchild2")); !errors.Is(err, core.ErrPlanStale) {
		t.Fatalf("不同改动应 ErrPlanStale：%v", err)
	}
	// 并发在同一张 READY 方案上做不同改动：只有一个成功，其余 ErrPlanStale
	p2 := insPlan(t, r, "itparent2", model.PlanReady)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, stale := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := r.SupersedePlan(ctx, p2.ID, childPlan(p2, fmt.Sprintf("itrace%d", i)))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, core.ErrPlanStale):
				stale++
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || stale != 5 {
		t.Fatalf("并发改同一张：成功 %d、STALE %d", ok, stale)
	}
	// 已确认 / 占用中的方案：不出子方案
	p3 := insPlan(t, r, "itparent3", model.PlanConfirmed)
	if _, _, err := r.SupersedePlan(ctx, p3.ID, childPlan(p3, "itchild3")); !errors.Is(err, core.ErrPlanConfirming) {
		t.Fatalf("已确认方案应 ErrPlanConfirming：%v", err)
	}
	if kids, _ := r.ChildPlans(ctx, p3.ID); len(kids) != 0 {
		t.Fatal("不得插入子方案")
	}
}

func savedFor(planID int64) *model.SavedPlan {
	return &model.SavedPlan{UserID: 990077, Name: fmt.Sprintf("方案%d", planID), PlanID: planID, PlanNo: fmt.Sprintf("VRP%d", planID), AnalysisID: 1,
		Annotations: []byte(`{}`), ModelGroup: "vid.x", Resolution: "720p"}
}

func TestIntegrationSavePlanLimitConcurrent(t *testing.T) {
	r, db := integrationRepo(t)
	libClean(t, db)
	for i := 0; i < model.MaxSavedPlans-2; i++ {
		if _, created, err := r.SavePlan(ctx, savedFor(int64(500000+i)), model.MaxSavedPlans, false); err != nil || !created {
			t.Fatalf("预置第 %d 个：%v %v", i, created, err)
		}
	}
	// 10 个不同方案并发保存，只剩 2 个名额
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, full := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, created, err := r.SavePlan(ctx, savedFor(int64(600000+i)), model.MaxSavedPlans, false)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && created:
				ok++
			case errors.Is(err, core.ErrSavedPlanLimit):
				full++
			default:
				t.Errorf("意外：%v %v", created, err)
			}
		}(i)
	}
	wg.Wait()
	_, total, _ := r.ListSavedPlans(ctx, 990077, 0, 100)
	if ok != 2 || full != 8 || total != int64(model.MaxSavedPlans) {
		t.Fatalf("并发保存不得超上限：成功 %d、满 %d、总数 %d", ok, full, total)
	}
	// 已保存过的方案再存：不占名额，rename 才改名
	got, created, err := r.SavePlan(ctx, &model.SavedPlan{UserID: 990077, Name: "新名字", PlanID: 500000, PlanNo: "x", Annotations: []byte(`{}`)}, model.MaxSavedPlans, true)
	if err != nil || created || got.Name != "新名字" {
		t.Fatalf("再存只改名：%+v %v %v", got, created, err)
	}
	// 同一方案并发保存：只存一份
	if ok, _ := r.DeleteSavedPlan(ctx, 990077, got.ID); !ok {
		t.Fatal("删除")
	}
	if ok, _ := r.DeleteSavedPlan(ctx, 990078, got.ID); ok {
		t.Fatal("别人的删不掉")
	}
	n := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := r.SavePlan(ctx, savedFor(700000), model.MaxSavedPlans, false)
			if err != nil {
				t.Error(err)
			}
			if created {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n != 1 {
		t.Fatalf("同一方案并发保存只建一份：%d", n)
	}
	if sp, _ := r.GetSavedPlan(ctx, 990078, got.ID); sp != nil {
		t.Fatal("越权读不到")
	}
	if err := r.TouchSavedPlan(ctx, 990077, got.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationSampleAnalysis(t *testing.T) {
	r, db := integrationRepo(t)
	libClean(t, db)
	smp := &model.Sample{Title: "it", Industry: "food", SourceRid: "itlib-src", Enabled: true, CreatedBy: 990077}
	if err := r.InsertSample(ctx, smp); err != nil {
		t.Fatal(err)
	}
	build := func(round int, rid string) *model.Analysis {
		a := core.NewSampleAnalysis(smp.ID, round, rid)
		a.SourceURL = "mr://" + rid
		return a
	}
	a1, busy, err := r.BeginSampleAnalysis(ctx, smp.ID, 1, build)
	if err != nil || busy || a1 == nil || a1.UserID != 0 {
		t.Fatalf("开第一轮：%+v %v %v", a1, busy, err)
	}
	if _, busy, _ := r.BeginSampleAnalysis(ctx, smp.ID, 1, build); !busy {
		t.Fatal("上一轮还在跑应 busy")
	}
	// 系统分析不进用户视角
	if got, _ := r.GetAnalysisByNo(ctx, 990077, a1.AnalysisNo); got != nil {
		t.Fatal("用户按编号拿不到系统分析")
	}
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_analysis SET status = 2 WHERE id = ?", a1.ID)
	if ok, _ := r.PromoteSampleAnalysis(ctx, smp.ID, a1.ID+999); ok {
		t.Fatal("pending 不匹配不能发布")
	}
	if ok, err := r.PromoteSampleAnalysis(ctx, smp.ID, a1.ID); err != nil || !ok {
		t.Fatalf("发布：%v %v", ok, err)
	}
	list, err := r.ListVisibleSamples(ctx, "food", 10)
	if err != nil || len(list) != 1 || list[0].AnalysisID == nil || *list[0].AnalysisID != a1.ID || list[0].PendingAnalysisID != nil || list[0].AnalyzedAt == nil {
		t.Fatalf("发布后：%+v %v", list, err)
	}
	if l2, _ := r.ListVisibleSamples(ctx, "toy", 10); len(l2) != 0 {
		t.Fatal("别的行业看不到")
	}
	// 第二轮失败：已发布的不动
	a2, busy, err := r.BeginSampleAnalysis(ctx, smp.ID, 1, build)
	if err != nil || busy || a2.SourceKey == a1.SourceKey {
		t.Fatalf("第二轮是新的一行：%+v %v %v", a2, busy, err)
	}
	if ok, _ := r.FailSampleAnalysis(ctx, smp.ID, a2.ID, "原视频太短了"); !ok {
		t.Fatal("失败")
	}
	cur, _ := r.GetSample(ctx, smp.ID)
	if *cur.AnalysisID != a1.ID || cur.PendingAnalysisID != nil || deref(cur.AnalysisError) != "原视频太短了" || cur.AnalysisRound != 2 {
		t.Fatalf("失败只记原因：%+v", cur)
	}
	// 换原片：清掉分析
	if ok, err := r.UpdateSample(ctx, smp.ID, map[string]any{"source_rid": "itlib-src2", "cover_rid": nil}, true); err != nil || !ok {
		t.Fatalf("改：%v %v", ok, err)
	}
	cur, _ = r.GetSample(ctx, smp.ID)
	if cur.AnalysisID != nil || cur.SourceRid != "itlib-src2" {
		t.Fatalf("换原片清掉分析：%+v", cur)
	}
	if rows, total, err := r.AdminListSamples(ctx, SampleFilter{Industry: "food"}, 0, 10); err != nil || total < 1 || len(rows) < 1 {
		t.Fatalf("后台列表：%v %v", total, err)
	}
	m, _ := r.ExistingAnalyses(ctx, []int64{a1.ID, a2.ID, 99999999})
	if !m[a1.ID] || !m[a2.ID] || m[99999999] {
		t.Fatalf("ExistingAnalyses：%v", m)
	}
	if ok, _ := r.DeleteSample(ctx, smp.ID, 1); !ok {
		t.Fatal("删除")
	}
	if l3, _ := r.ListVisibleSamples(ctx, "food", 10); len(l3) != 0 {
		t.Fatal("删除后用户看不到")
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// 确认没走完放回方案：占用期间有子方案 → STALE + superseded_by；没有 → READY。
func TestIntegrationReleasePlanWithChild(t *testing.T) {
	r, _ := integrationRepo(t)
	p := insPlan(t, r, "itrel1", model.PlanReady)
	if ok, _ := r.ConfirmPlan(ctx, p.ID, 77); !ok {
		t.Fatal("占用")
	}
	// 旧数据：占用期间已有两张 READY 子方案
	var kids []*model.Plan
	for i := 0; i < 2; i++ {
		c, created, err := r.InsertPlan(ctx, childPlan(p, fmt.Sprintf("itrelchild%d", i)))
		if err != nil || !created {
			t.Fatal(err)
		}
		kids = append(kids, c)
	}
	if ok, err := r.ReleasePlan(ctx, p.ID, 77); err != nil || !ok {
		t.Fatalf("放回：%v %v", ok, err)
	}
	cur, _ := r.GetPlan(ctx, p.ID)
	old, _ := r.GetPlan(ctx, kids[0].ID)
	newest, _ := r.GetPlan(ctx, kids[1].ID)
	if cur.Status != model.PlanStale || cur.SupersededBy == nil || *cur.SupersededBy != newest.ID || cur.JobID != nil {
		t.Fatalf("有子方案应 STALE：%+v", cur)
	}
	if old.Status != model.PlanStale || old.SupersededBy == nil || *old.SupersededBy != newest.ID || newest.Status != model.PlanReady {
		t.Fatalf("READY 子方案只留最新一张：old=%+v newest=%+v", old, newest)
	}
	p2 := insPlan(t, r, "itrel2", model.PlanReady)
	_, _ = r.ConfirmPlan(ctx, p2.ID, 78)
	if ok, err := r.ReleasePlan(ctx, p2.ID, 78); err != nil || !ok {
		t.Fatalf("放回：%v %v", ok, err)
	}
	if cur, _ := r.GetPlan(ctx, p2.ID); cur.Status != model.PlanReady || cur.SupersededBy != nil {
		t.Fatalf("没有子方案应 READY：%+v", cur)
	}
}
