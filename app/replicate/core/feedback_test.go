package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"
)

// 内存版反馈表：(user, job, seg) 唯一，覆盖更新。
type memFeedback struct {
	mu   sync.Mutex
	rows map[string]*model.Feedback
	fail error
}

func (m *memFeedback) key(u, j int64, s int) string { return fmt.Sprintf("%d/%d/%d", u, j, s) }

func (m *memFeedback) UpsertFeedback(_ context.Context, f *model.Feedback) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	if m.rows == nil {
		m.rows = map[string]*model.Feedback{}
	}
	k := m.key(f.UserID, f.JobID, f.SegIndex)
	_, existed := m.rows[k]
	c := *f
	m.rows[k] = &c
	return !existed, nil
}

func (m *memFeedback) GetFeedback(_ context.Context, u, j int64, s int) (*model.Feedback, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[m.key(u, j, s)]; ok {
		c := *r
		return &c, nil
	}
	return nil, nil
}

func (m *memFeedback) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.rows) }

// fbEnv 带反馈 / 审核 / 漏斗的场景，已有一条做好的三段 job（smooth 或 fast）。
func fbEnv(t *testing.T, smooth bool) (*env, *memFeedback, *fakeFunnel, int64) {
	ops := newMemOps(nil)
	fb, f := &memFeedback{}, &fakeFunnel{}
	e := newOpsEnv(t, true, nil, func(*env) Option { return WithModeration(ops, ops, fakeWords{hit: "违禁词"}) },
		func(*env) Option { return WithFeedback(fb) }, func(*env) Option { return WithFunnel(f) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("pass")
	e.gw.analysis[opModerationText] = verdicts("pass")
	var pv *PlanView
	if smooth {
		pv = e.smoothPlan(1)
	} else {
		pv = e.plan(e.seedAnalysis(32, 12, 25))
	}
	jv := e.run(e.confirm(pv, "all", "k1").JobID, 10)
	if jv.Status != "succeeded" {
		t.Fatalf("前置：%+v", jv)
	}
	return e, fb, f, jv.JobID
}

func TestFeedbackUpsertOverwritesAndRecordsContinuity(t *testing.T) {
	e, fb, f, job := fbEnv(t, true)
	v, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "down", Reasons: []string{"seam_jump", "other", "seam_jump"}, SegIndex: 2, Note: "  第二段\n开头跳了 "})
	if err != nil {
		t.Fatalf("提交：%v", err)
	}
	if v.Updated || v.Continuity != ContinuitySmooth || v.Rating != "down" || fmt.Sprint(v.Reasons) != "[seam_jump other]" || v.Note != "第二段 开头跳了" || v.Message != "谢谢，已记录" {
		t.Fatalf("回执：%+v", v)
	}
	// 同一用户同一任务同一段再次提交：覆盖，不新增行
	v, err = e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "up", Reasons: []string{"face_mismatch"}, SegIndex: 2})
	if err != nil || !v.Updated || fb.count() != 1 {
		t.Fatalf("应覆盖更新：%v %+v 行数=%d", err, v, fb.count())
	}
	row, _ := fb.GetFeedback(e.ctx, uid, job, 2)
	if row.Rating != "up" || len(row.Reasons) != 0 || row.Note != "" || row.Continuity != ContinuitySmooth {
		t.Fatalf("up 不收原因，覆盖后备注清空：%+v", row)
	}
	// 整条成片（seg 0）与第 2 段是两行
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "up"}); err != nil || fb.count() != 2 {
		t.Fatalf("整条成片另一行：%v %d", err, fb.count())
	}
	// 漏斗：down 一次、up（换了评价）一次、整条 up 一次；重复提交同评价不再记
	_, _ = e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "up", SegIndex: 2})
	var got []string
	for _, r := range f.rows {
		if strings.HasPrefix(r.Stage, "feedback_") {
			got = append(got, fmt.Sprintf("%s/%d/%s", r.Stage, r.SegIndex, r.Continuity))
		}
	}
	if fmt.Sprint(got) != "[feedback_down/2/smooth feedback_up/2/smooth feedback_up/0/smooth]" {
		t.Fatalf("漏斗事件：%v", got)
	}
}

func TestFeedbackFastJobRecordsFast(t *testing.T) {
	e, fb, _, job := fbEnv(t, false)
	v, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "up"})
	if err != nil || v.Continuity != ContinuityFast {
		t.Fatalf("%v %+v", err, v)
	}
	if r, _ := fb.GetFeedback(e.ctx, uid, job, 0); r == nil || r.Continuity != ContinuityFast {
		t.Fatalf("%+v", r)
	}
}

func TestFeedbackValidation(t *testing.T) {
	e, fb, _, job := fbEnv(t, false)
	cases := []struct {
		name string
		req  FeedbackReq
	}{
		{"评价为空", FeedbackReq{Rating: ""}},
		{"评价非法", FeedbackReq{Rating: "meh"}},
		{"原因非法", FeedbackReq{Rating: "down", Reasons: []string{"bad"}}},
		{"段号为负", FeedbackReq{Rating: "down", SegIndex: -1}},
		{"段号越界", FeedbackReq{Rating: "down", SegIndex: 4}},
		{"备注超长", FeedbackReq{Rating: "down", Note: strings.Repeat("字", 201)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.UserID, c.req.JobID = uid, job
			_, err := e.svc.SubmitFeedback(e.ctx, c.req)
			if code, _, ok := xerr.Parse(err); !ok || code != xerr.ParamError {
				t.Fatalf("应参数错误：%v", err)
			}
		})
	}
	if fb.count() != 0 {
		t.Fatal("非法请求不得落库")
	}
	// 恰好 200 字可以；原因枚举全部合法
	all := []string{}
	for _, r := range FeedbackReasons {
		all = append(all, r.Code)
	}
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "down", Reasons: all, Note: strings.Repeat("字", 200)}); err != nil {
		t.Fatalf("枚举全选 + 200 字应通过：%v", err)
	}
}

func TestFeedbackOwnershipAndState(t *testing.T) {
	e, fb, _, job := fbEnv(t, false)
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid + 1, JobID: job, Rating: "up"}); err == nil {
		t.Fatal("别人的任务应被拒")
	} else if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("别人的任务应 NotFound：%v", err)
	}
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: 99999, Rating: "up"}); err == nil {
		t.Fatal("不存在的任务应报错")
	}
	// 还没做好的任务（刚确认、没有任何段做完）
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: e.seedAnalysis(10).AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	jv := e.confirm(pv, "all", "k2")
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: jv.JobID, Rating: "up"}); reasonOf(err) != ReasonJobNotFinished {
		t.Fatalf("没做好应 JOB_NOT_FINISHED：%v", err)
	}
	if fb.count() != 0 {
		t.Fatal("被拒的不得落库")
	}
	// 没装配
	e2 := newEnv(t, true)
	if _, err := e2.svc.SubmitFeedback(e2.ctx, FeedbackReq{UserID: uid, JobID: 1, Rating: "up"}); err == nil {
		t.Fatal("没装配应 7003")
	} else if code, _, _ := xerr.Parse(err); code != xerr.NotReady {
		t.Fatalf("%v", err)
	}
}

// 备注过文本审核：关键词 / 网关判违规 → MODERATION_REJECTED（不落库）；审核服务故障 → 评价与原因照常记录，只丢备注。
func TestFeedbackNoteModeration(t *testing.T) {
	e, fb, _, job := fbEnv(t, false)
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "down", Note: "含违禁词的话"}); reasonOf(err) != ReasonModerationRejected {
		t.Fatalf("关键词应拒：%v", err)
	}
	e.gw.analysis[opModerationText] = verdicts("block")
	_, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "down", Note: "网关判违规"})
	if reasonOf(err) != ReasonModerationRejected || strings.Contains(err.Error(), "porn") {
		t.Fatalf("网关判违规应拒且不暴露标签：%v", err)
	}
	if fb.count() != 0 {
		t.Fatal("违规备注不得落库")
	}
	e.gw.analysis[opModerationText] = verdicts("pass")
	if v, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "down", Reasons: []string{"motion_weird"}, Note: "动作怪"}); err != nil || v.Note != "动作怪" {
		t.Fatalf("通过：%v %+v", err, v)
	}
	// 没有备注时不调审核（审核挂了也不影响纯评价）
	e.gw.submitErr = errors.New("网关故障")
	if v, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "up", SegIndex: 1}); err != nil || v.NoteDropped {
		t.Fatalf("无备注不审核：%v %+v", err, v)
	}
	// 审核服务故障（fail-close）：不拒收评价，只丢备注并如实告知
	v, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "down", Reasons: []string{"face_mismatch"}, SegIndex: 2, Note: "有话说"})
	if err != nil || !v.NoteDropped || v.Note != "" || fmt.Sprint(v.Reasons) != "[face_mismatch]" {
		t.Fatalf("审核故障应只丢备注：%v %+v", err, v)
	}
	if r, _ := fb.GetFeedback(e.ctx, uid, job, 2); r == nil || r.Note != "" || r.Rating != "down" {
		t.Fatalf("评价应已记录：%+v", r)
	}
}

func TestFeedbackStoreErrorSurfaces(t *testing.T) {
	e, fb, _, job := fbEnv(t, false)
	fb.fail = errors.New("库故障")
	if _, err := e.svc.SubmitFeedback(e.ctx, FeedbackReq{UserID: uid, JobID: job, Rating: "up"}); err == nil {
		t.Fatal("写库失败应报错，不能假装记录了")
	}
}
