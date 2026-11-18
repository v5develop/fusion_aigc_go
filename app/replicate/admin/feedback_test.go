package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/xerr"
)

// fbStore 在 fakeStore 上加方案 / 分析 / 反馈（只覆盖 §AB 用到的方法）。
type fbStore struct {
	*fakeStore
	plan      *model.Plan
	analysis  *model.Analysis
	feedbacks []*model.Feedback
	gotFilter repo.FeedbackFilter
	gotOff    int
	gotLimit  int
	stats     *repo.FeedbackStats
	gotFrom   time.Time
	gotTo     time.Time
}

func (f *fbStore) AdminGetPlan(context.Context, int64) (*model.Plan, error) { return f.plan, nil }
func (f *fbStore) GetAnalysis(context.Context, int64) (*model.Analysis, error) {
	return f.analysis, nil
}
func (f *fbStore) AdminListFeedback(_ context.Context, fl repo.FeedbackFilter, off, limit int) ([]*model.Feedback, int64, error) {
	f.gotFilter, f.gotOff, f.gotLimit = fl, off, limit
	return f.feedbacks, int64(len(f.feedbacks)), nil
}
func (f *fbStore) AdminFeedbackSummary(_ context.Context, from, to time.Time) (*repo.FeedbackStats, error) {
	f.gotFrom, f.gotTo = from, to
	return f.stats, nil
}
func (f *fbStore) ListJobFeedback(context.Context, int64) ([]*model.Feedback, error) {
	return f.feedbacks, nil
}

func fbSetup(t *testing.T) (*Service, *fbStore) {
	t.Helper()
	_, st, _ := setup()
	an := core.AnalysisResult{DimOrder: []string{"model"}, Dims: map[string]core.Dim{"model": {Present: true}},
		Subjects: []core.Subject{{ID: "s1", Name: "左侧白衣女生"}, {ID: "s2", Name: "男生"}}}
	ab, _ := json.Marshal(an)
	ann := core.Annotations{Dims: map[string]core.DimEdit{"model": {Mode: core.ModeReplace,
		Subjects: []core.SubjectEdit{{ID: "s1", Mode: core.ModeReplace, Refs: []int{1}}}}}}
	annB, _ := json.Marshal(ann)
	f := &fbStore{fakeStore: st, plan: &model.Plan{ID: 5, AnalysisID: 9, Annotations: annB}, analysis: &model.Analysis{ID: 9, Result: ab}}
	fixed := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return fixed }
	svc := New(f, st.bill, &fakeForcer{})
	svc.now = func() time.Time { return fixed }
	return svc, f
}

// 详情：smooth 任务带 continuity、第 2 段起的 bridge、按人点名说明与用户反馈；第 1 段不带 bridge。
func TestDetailContinuityBridgeSubjectsFeedback(t *testing.T) {
	s, f := fbSetup(t)
	f.job.Input = json.RawMessage(`{"continuity":"smooth"}`)
	f.steps[0].Output = json.RawMessage(`{"bridgeSkip":"refs_full"}`) // 第 1 段：即使有也不展示
	f.steps[1].Output = json.RawMessage(`{"bridgeRid":"rid_x"}`)
	f.steps[2].Output = json.RawMessage(`{"bridgeSkip":"prev_failed"}`)
	f.feedbacks = []*model.Feedback{{ID: 1, UserID: 7, JobID: 42, SegIndex: 0, Rating: "down", Reasons: json.RawMessage(`["seam_jump","other"]`),
		Note: "接缝很跳", Continuity: "smooth", UpdatedAt: time.Now()}}
	d, err := s.Detail(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if d.Job.Continuity != "smooth" {
		t.Fatalf("continuity=%q", d.Job.Continuity)
	}
	if got := []string{d.Segments[0].Bridge, d.Segments[1].Bridge, d.Segments[2].Bridge}; got[0] != "" || got[1] != "used" || got[2] != "skipped_prev_failed" {
		t.Fatalf("bridge=%v", got)
	}
	if d.Plan == nil || len(d.Plan.SubjectText) != 1 || d.Plan.SubjectText[0] != "人物：左侧白衣女生 → 图 1；男生 保持不变" {
		t.Fatalf("subjectText=%+v", d.Plan)
	}
	if len(d.Feedback) != 1 || d.Feedback[0].Rating != "down" || len(d.Feedback[0].Reasons) != 2 || d.Feedback[0].Note != "接缝很跳" {
		t.Fatalf("feedback=%+v", d.Feedback)
	}
	b, _ := json.Marshal(d)
	for _, k := range []string{`"continuity":"smooth"`, `"bridge":"used"`, `"subjectText":["人物：左侧白衣女生`, `"feedback":[{`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON 缺 %s", k)
		}
	}
}

// 快速模式 / 没有点名 / 没有反馈：新字段全部不出现（只增、omitempty）；fast 任务不带 bridge，即使步骤里有。
func TestDetailNewFieldsOmittedWhenAbsent(t *testing.T) {
	s, f := fbSetup(t)
	f.steps[1].Output = json.RawMessage(`{"bridgeRid":"rid_x"}`)
	f.plan.Annotations = json.RawMessage(`{"dims":{}}`)
	d, err := s.Detail(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if d.Job.Continuity != "fast" || d.Segments[1].Bridge != "" || d.Plan != nil || d.Feedback != nil {
		t.Fatalf("%+v", d)
	}
	b, _ := json.Marshal(d)
	for _, k := range []string{`"bridge"`, `"subjectText"`, `"feedback"`, `"plan"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("不该出现 %s", k)
		}
	}
}

func TestListFeedbackDefaultsAndLimits(t *testing.T) {
	s, f := fbSetup(t)
	f.feedbacks = []*model.Feedback{{ID: 3, UserID: 12345678, JobID: 42, Rating: "up", Continuity: "fast"},
		{ID: 2, UserID: 7, JobID: 42, SegIndex: 1, Rating: "down", Reasons: json.RawMessage(`null`), Continuity: "smooth"}}
	out, err := s.ListFeedback(ctx, FeedbackListReq{Size: 5000, Page: 3})
	if err != nil {
		t.Fatal(err)
	}
	if f.gotLimit != 100 || f.gotOff != 200 || out.Size != 100 || out.Page != 3 {
		t.Fatalf("分页夹紧：limit=%d off=%d %+v", f.gotLimit, f.gotOff, out)
	}
	if f.gotFilter.To == nil || f.gotFilter.From == nil || f.gotFilter.To.Sub(*f.gotFilter.From) != 30*24*time.Hour {
		t.Fatalf("默认窗口应为 30 天：%+v", f.gotFilter)
	}
	if out.Items[0].UserIDMasked != "12****78" || out.Items[1].Reasons == nil || len(out.Items[1].Reasons) != 0 {
		t.Fatalf("行：%+v", out.Items)
	}
	// 默认页大小
	if _, err := s.ListFeedback(ctx, FeedbackListReq{}); err != nil || f.gotLimit != 20 || f.gotOff != 0 {
		t.Fatalf("默认 20 / 第 1 页：%d %d %v", f.gotLimit, f.gotOff, err)
	}
	// 透传筛选
	if _, err := s.ListFeedback(ctx, FeedbackListReq{Rating: "down", Reason: "seam_jump", Continuity: "smooth", UserID: 7, JobID: 42}); err != nil ||
		f.gotFilter.Rating != "down" || f.gotFilter.Reason != "seam_jump" || f.gotFilter.Continuity != "smooth" || f.gotFilter.UserID != 7 || f.gotFilter.JobID != 42 {
		t.Fatalf("筛选透传：%+v %v", f.gotFilter, err)
	}
}

func TestListFeedbackValidation(t *testing.T) {
	s, _ := fbSetup(t)
	d := func(y, m, day int) *time.Time { v := time.Date(y, time.Month(m), day, 0, 0, 0, 0, time.UTC); return &v }
	cases := map[string]FeedbackListReq{
		"rating":     {Rating: "meh"},
		"reason":     {Reason: "drop table"},
		"continuity": {Continuity: "slow"},
		"from 早于 to": {From: d(2026, 11, 2), To: d(2026, 11, 1)},
		"180 天":      {From: d(2026, 1, 1), To: d(2026, 8, 1)},
	}
	for name, req := range cases {
		_, err := s.ListFeedback(ctx, req)
		if c, _, _ := xerr.Parse(err); err == nil || c != xerr.ParamError {
			t.Errorf("%s 应参数错误：%v", name, err)
		}
	}
	// 恰好 180 天放行；只给 from 到现在（11-18），跨度超限同样拒绝
	if _, err := s.ListFeedback(ctx, FeedbackListReq{From: d(2026, 1, 1), To: d(2026, 6, 30)}); err != nil {
		t.Fatalf("180 天内应放行：%v", err)
	}
	if _, err := s.ListFeedback(ctx, FeedbackListReq{From: d(2026, 1, 1)}); err == nil {
		t.Fatal("只给 from 且到现在超过 180 天应拒绝")
	}
}

func TestFeedbackSummaryShape(t *testing.T) {
	s, f := fbSetup(t)
	f.stats = &repo.FeedbackStats{Total: 10, Up: 6, Down: 4, UpSmooth: 3, DownSmooth: 3, UpFast: 3, DownFast: 1,
		Reasons: []repo.ReasonStat{{Reason: "seam_jump", Count: 3, Smooth: 3}, {Reason: "other", Count: 1, Fast: 1}}}
	out, err := s.FeedbackSummary(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.gotTo.Sub(f.gotFrom) != 30*24*time.Hour || out.Total != 10 || out.Up != 6 || out.Down != 4 {
		t.Fatalf("%+v", out)
	}
	if len(out.ByContinuity) != 2 || out.ByContinuity[0] != (ContinuityCount{"fast", 3, 1}) || out.ByContinuity[1] != (ContinuityCount{"smooth", 3, 3}) {
		t.Fatalf("byContinuity=%+v", out.ByContinuity)
	}
	if out.ByReason[0] != (ReasonCount{"seam_jump", 3, 3, 0}) {
		t.Fatalf("byReason=%+v", out.ByReason)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"byReason":[{"reason":"seam_jump","count":3,"smooth":3,"fast":0}`) {
		t.Fatalf("JSON：%s", b)
	}
	// Store 不支持反馈时回未开放
	plain, _, _ := setup()
	if _, err := plain.FeedbackSummary(ctx, nil, nil); err == nil {
		t.Fatal("没装配应回未开放")
	}
}
