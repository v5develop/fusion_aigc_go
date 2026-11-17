package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"
)

// ---- 替身：与 repo/share.go 同语义的内存投稿库 ----

type memShares struct {
	mu       sync.Mutex
	id       int64
	rows     map[int64]*model.Share
	industry map[int64]string
	// 撤回已通过的投稿时被下架的示例号
	unpublished []int64
}

func newMemShares() *memShares {
	return &memShares{rows: map[int64]*model.Share{}, industry: map[int64]string{}}
}

func (m *memShares) GetShare(_ context.Context, id int64) (*model.Share, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}
func (m *memShares) GetShareByJob(_ context.Context, jobID int64) (*model.Share, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.JobID == jobID {
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}
func (m *memShares) InsertShare(_ context.Context, sh *model.Share) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.JobID == sh.JobID {
			return false, nil
		}
	}
	m.id++
	c := *sh
	c.ID = m.id
	m.rows[c.ID] = &c
	sh.ID = c.ID
	return true, nil
}
func (m *memShares) ReopenShare(_ context.Context, id int64, consent bool, industry string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil && r.Status == model.ShareWithdrawn {
		r.Status, r.PortraitConsent, r.Industry, r.SampleID, r.RejectReason = model.SharePending, consent, industry, nil, ""
		return true, nil
	}
	return false, nil
}
func (m *memShares) WithdrawShare(_ context.Context, userID, id int64) (*model.Share, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[id]
	if r == nil || r.UserID != userID {
		return nil, false, nil
	}
	prev := *r
	if r.Status != model.SharePending && r.Status != model.ShareApproved {
		return &prev, false, nil
	}
	r.Status = model.ShareWithdrawn
	if prev.Status == model.ShareApproved && prev.SampleID != nil {
		m.unpublished = append(m.unpublished, *prev.SampleID)
	}
	return &prev, true, nil
}
func (m *memShares) UserIndustry(_ context.Context, uid int64) (string, error) {
	return m.industry[uid], nil
}

// shareEnv 一条已交付完整成片 + 内存投稿库 + 漏斗。
type shareEnv struct {
	*pubEnv
	sh *memShares
	ff *fakeFunnel
}

func newShareEnv(t *testing.T) *shareEnv {
	e := newPubEnv(t)
	sh := newMemShares()
	e.svc.shares = sh
	ff := &fakeFunnel{keys: map[string]bool{}}
	e.svc.funnelStore = ff
	return &shareEnv{e, sh, ff}
}

// setAnnotations 改成片所用方案的改法（模拟「替换人物」「只换商品」等）。
func (e *shareEnv) setAnnotations(a Annotations) {
	b, _ := json.Marshal(a)
	for _, p := range e.store.plans {
		if p.ID == e.job().PlanID {
			p.Annotations = b
		}
	}
}

func replaceModel() Annotations {
	return Annotations{Dims: map[string]DimEdit{"model": {Mode: ModeReplace, Refs: []int{1}}}, Audio: AudioKeep}
}

func TestShareSampleCreatesPendingAndReplays(t *testing.T) {
	e := newShareEnv(t)
	e.sh.industry[uid] = "food"
	v, err := e.svc.ShareSample(e.ctx, uid, e.jobID, false) // 默认方案只换商品：不需要肖像授权
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != model.SharePending || v.SubmissionID == 0 || v.Replay || v.StatusLabel != "待审核" || v.JobID != e.jobID {
		t.Fatalf("新投稿：%+v", v)
	}
	row, _ := e.sh.GetShare(e.ctx, v.SubmissionID)
	if row.UserID != uid || row.PlanID != e.job().PlanID || row.Industry != "food" || row.RewardStatus != model.RewardNone {
		t.Fatalf("落库：%+v", row)
	}
	again, err := e.svc.ShareSample(e.ctx, uid, e.jobID, false)
	if err != nil || !again.Replay || again.SubmissionID != v.SubmissionID || len(e.sh.rows) != 1 {
		t.Fatalf("同一成片只能投一次，重复调用回放：%+v err=%v rows=%d", again, err, len(e.sh.rows))
	}
	if e.ff.count(StageShareSample) != 1 {
		t.Fatalf("漏斗 share_sample=%d（回放不重复记）", e.ff.count(StageShareSample))
	}
}

func TestShareSamplePortraitConsent(t *testing.T) {
	e := newShareEnv(t)
	e.setAnnotations(replaceModel())
	_, err := e.svc.ShareSample(e.ctx, uid, e.jobID, false)
	if xerr.ReasonOf(err) != ReasonPortraitConsentRequired || len(e.sh.rows) != 0 {
		t.Fatalf("替换人物没带授权应拒收且不建投稿：%v rows=%d", err, len(e.sh.rows))
	}
	v, err := e.svc.ShareSample(e.ctx, uid, e.jobID, true)
	if err != nil || !v.PortraitConsent {
		t.Fatalf("带授权应建投稿并记录：%+v err=%v", v, err)
	}
	if r, _ := e.sh.GetShare(e.ctx, v.SubmissionID); !r.PortraitConsent {
		t.Fatal("授权应落库")
	}
	// 描述式改人物（不是换成别人的肖像）不要求授权
	e2 := newShareEnv(t)
	e2.setAnnotations(Annotations{Dims: map[string]DimEdit{"model": {Mode: ModeDescribe, Note: "年轻一点"}}})
	if _, err := e2.svc.ShareSample(e2.ctx, uid, e2.jobID, false); err != nil {
		t.Fatalf("describe 不要求肖像授权：%v", err)
	}
}

func TestShareSampleOnlyCompleteFinalOfOwner(t *testing.T) {
	e := newShareEnv(t)
	if _, err := e.svc.ShareSample(e.ctx, uid+1, e.jobID, true); err == nil {
		t.Fatal("别人的成片不能投")
	}
	for name, mut := range map[string]func(j *model.Job){
		"预览版": func(j *model.Job) {
			j.ResultDocument = json.RawMessage(`{"status":"ready","kind":"preview","videoRid":"x"}`)
		},
		"部分成片": func(j *model.Job) {
			j.ResultDocument = json.RawMessage(`{"status":"ready","kind":"partial","videoRid":"x"}`)
		},
		"部分成功": func(j *model.Job) { j.Status = model.JobPartial },
		"没成片":  func(j *model.Job) { j.ResultDocument = nil },
	} {
		e2 := newShareEnv(t)
		mut(e2.job())
		if _, err := e2.svc.ShareSample(e2.ctx, uid, e2.jobID, true); xerr.ReasonOf(err) != ReasonJobNotDelivered {
			t.Errorf("%s 应 JOB_NOT_DELIVERED：%v", name, err)
		}
		if len(e2.sh.rows) != 0 {
			t.Errorf("%s 不应建投稿", name)
		}
	}
}

func TestShareSampleWithdrawnCanReshareRejectedCannot(t *testing.T) {
	e := newShareEnv(t)
	v, _ := e.svc.ShareSample(e.ctx, uid, e.jobID, false)
	if _, err := e.svc.WithdrawSample(e.ctx, uid, v.SubmissionID); err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.ShareSample(e.ctx, uid, e.jobID, false)
	if err != nil || again.Replay || again.Status != model.SharePending || again.SubmissionID != v.SubmissionID {
		t.Fatalf("撤回后可重新投（同一行改回待审核）：%+v err=%v", again, err)
	}
	if e.ff.count(StageShareSample) != 1 {
		t.Fatalf("重新投稿不重复记 share_sample：%d", e.ff.count(StageShareSample))
	}
	e.sh.rows[v.SubmissionID].Status = model.ShareRejected
	if _, err := e.svc.ShareSample(e.ctx, uid, e.jobID, false); xerr.ReasonOf(err) != ReasonShareRejected {
		t.Fatalf("被驳回的不能再投：%v", err)
	}
}

func TestWithdrawSampleUnpublishesApproved(t *testing.T) {
	e := newShareEnv(t)
	v, _ := e.svc.ShareSample(e.ctx, uid, e.jobID, false)
	// 待审核撤回：不涉及示例
	w, err := e.svc.WithdrawSample(e.ctx, uid, v.SubmissionID)
	if err != nil || w.Status != model.ShareWithdrawn || w.Unpublished || w.Replay || len(e.sh.unpublished) != 0 {
		t.Fatalf("待审核撤回：%+v err=%v", w, err)
	}
	// 重复撤回：回放，不报错，漏斗不重复
	w, err = e.svc.WithdrawSample(e.ctx, uid, v.SubmissionID)
	if err != nil || !w.Replay || w.Unpublished || e.ff.count(StageWithdrawSample) != 1 {
		t.Fatalf("重复撤回应回放：%+v err=%v 漏斗=%d", w, err, e.ff.count(StageWithdrawSample))
	}
	// 已通过的：立即下架示例
	r := e.sh.rows[v.SubmissionID]
	sid := int64(77)
	r.Status, r.SampleID = model.ShareApproved, &sid
	w, err = e.svc.WithdrawSample(e.ctx, uid, v.SubmissionID)
	if err != nil || !w.Unpublished || w.Status != model.ShareWithdrawn || len(e.sh.unpublished) != 1 || e.sh.unpublished[0] != 77 {
		t.Fatalf("已通过的撤回应下架示例 77：%+v err=%v un=%v", w, err, e.sh.unpublished)
	}
	if !strings.Contains(w.Text, "下架") {
		t.Fatalf("文案应说明已下架：%s", w.Text)
	}
	if e.ff.count(StageWithdrawSample) != 1 { // 漏斗按投稿去重：同一投稿重新投再撤回不重复记
		t.Fatalf("同一投稿的撤回只记一次：%d", e.ff.count(StageWithdrawSample))
	}
}

func TestWithdrawSampleGuards(t *testing.T) {
	e := newShareEnv(t)
	v, _ := e.svc.ShareSample(e.ctx, uid, e.jobID, false)
	if _, err := e.svc.WithdrawSample(e.ctx, uid+1, v.SubmissionID); err == nil {
		t.Fatal("别人不能撤回")
	}
	if _, err := e.svc.WithdrawSample(e.ctx, uid, 9999); err == nil {
		t.Fatal("不存在的投稿应报找不到")
	}
	e.sh.rows[v.SubmissionID].Status = model.ShareRejected
	w, err := e.svc.WithdrawSample(e.ctx, uid, v.SubmissionID)
	if err != nil || !w.Replay || w.Status != model.ShareRejected || w.Unpublished {
		t.Fatalf("被驳回的不需要撤回，回放且不改状态：%+v err=%v", w, err)
	}
	e.svc.shares = nil
	if _, err := e.svc.WithdrawSample(e.ctx, uid, 1); err == nil {
		t.Fatal("没装配应 7003")
	} else if code, _, _ := xerr.Parse(err); code != xerr.NotReady {
		t.Fatalf("应 7003：%v", err)
	}
	if _, err := e.svc.ShareSample(e.ctx, uid, e.jobID, true); err == nil {
		t.Fatal("没装配应 7003")
	}
}

func TestShareChangeSummaryAndSampleAnnotationsStripUserContent(t *testing.T) {
	raw, _ := json.Marshal(Annotations{
		Dims: map[string]DimEdit{
			"model":   {Mode: ModeReplace, Note: "我的模特小红", Refs: []int{1, 2}},
			"scene":   {Mode: ModeDescribe, Note: "海边日落"},
			"garment": {Mode: ModeKeep, Note: "忽略"},
			"light":   {Mode: ModeReplace, Occurrences: []OccurrenceEdit{{Index: 1, Refs: []int{3}}}},
		},
		Audio: AudioRevoice, Texts: []TextOverlay{{Text: "我的店名"}},
		Voice: &VoiceEdit{VoiceID: 3, Lines: []VoiceLineEdit{{Index: 1, Text: "全场五折"}}},
	})
	sum := ShareChangeSummary(raw)
	if sum.Label != "换模特 + 换光线 + 改场景 + 换声音" && sum.Label != "换模特 + 改场景 + 换光线 + 换声音" {
		t.Fatalf("摘要：%q", sum.Label)
	}
	if sum.Audio != AudioRevoice || len(sum.Dims) != 3 {
		t.Fatalf("摘要 dims：%+v", sum)
	}
	if b, _ := json.Marshal(sum); strings.Contains(string(b), "小红") || strings.Contains(string(b), "海边") || strings.Contains(string(b), "店名") {
		t.Fatalf("摘要不含用户内容：%s", b)
	}
	clean := ShareSampleAnnotations(raw)
	for _, bad := range []string{"小红", "海边", "refs", "occurrences", "我的店名", "全场五折", "voice", "texts", "note"} {
		if strings.Contains(string(clean), bad) {
			t.Fatalf("公开示例的改法不应含 %q：%s", bad, clean)
		}
	}
	var got Annotations
	if err := json.Unmarshal(clean, &got); err != nil || got.Audio != AudioRegenerate || got.Dims["model"].Mode != ModeReplace ||
		got.Dims["scene"].Mode != ModeDescribe || len(got.Dims) != 3 {
		t.Fatalf("保留项名与处理方式、换声音退回默认声音：%+v err=%v", got, err)
	}
	if _, err := ValidSampleAnnotations(clean); err != nil {
		t.Fatalf("生成的示例改法必须过示例登记的结构校验：%v", err)
	}
	if s := ShareChangeSummary(nil); s.Label != "未做改动" || s.Dims == nil {
		t.Fatalf("空改法：%+v", s)
	}
	if ShareSampleAnnotations(json.RawMessage(`oops`)) != nil {
		t.Fatal("读不出来返回 nil")
	}
}

func TestNewSharedAnalysisIsSystemRowAndDeterministic(t *testing.T) {
	rid := "src-rid"
	fin := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	src := &model.Analysis{UserID: 7, SourceURL: "https://x/v.mp4", SourceRid: &rid, Status: model.AnalysisSucceeded,
		State: json.RawMessage(`{"motion":[1]}`), Result: json.RawMessage(`{"durationS":10}`), FinishedAt: &fin}
	a, b := NewSharedAnalysis(5, src), NewSharedAnalysis(5, src)
	if a.UserID != model.SystemUserID || a.Status != model.AnalysisSucceeded || a.Stage != model.StageDone || a.SourceKey != b.SourceKey ||
		string(a.Result) != `{"durationS":10}` || string(a.State) != `{"motion":[1]}` || *a.SourceRid != rid || a.AnalysisNo == b.AnalysisNo {
		t.Fatalf("系统身份的整行复制：%+v", a)
	}
	if NewSharedAnalysis(6, src).SourceKey == a.SourceKey {
		t.Fatal("不同投稿 source_key 应不同")
	}
}
