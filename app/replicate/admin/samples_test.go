package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/xerr"
)

// fakeSamples 内存 SampleStore（嵌入 Store 只为满足 New 的签名，示例管理不调它的方法）。
type fakeSamples struct {
	Store
	id       int64
	samples  map[int64]*model.Sample
	analyses map[int64]*model.Analysis
}

func newFakeSamples() *fakeSamples {
	return &fakeSamples{samples: map[int64]*model.Sample{}, analyses: map[int64]*model.Analysis{}}
}

func scp[T any](p *T) *T { c := *p; return &c }

func (f *fakeSamples) GetAnalysis(_ context.Context, id int64) (*model.Analysis, error) {
	if a := f.analyses[id]; a != nil {
		return cp(a), nil
	}
	return nil, nil
}
func (f *fakeSamples) GetSample(_ context.Context, id int64) (*model.Sample, error) {
	if s := f.samples[id]; s != nil {
		return cp(s), nil
	}
	return nil, nil
}
func (f *fakeSamples) PromoteSampleAnalysis(_ context.Context, sid, pid int64) (bool, error) {
	s := f.samples[sid]
	if s == nil || s.PendingAnalysisID == nil || *s.PendingAnalysisID != pid {
		return false, nil
	}
	s.AnalysisID, s.PendingAnalysisID, s.AnalysisError = &pid, nil, nil
	return true, nil
}
func (f *fakeSamples) FailSampleAnalysis(_ context.Context, sid, pid int64, reason string) (bool, error) {
	s := f.samples[sid]
	if s == nil || s.PendingAnalysisID == nil || *s.PendingAnalysisID != pid {
		return false, nil
	}
	s.PendingAnalysisID, s.AnalysisError = nil, &reason
	return true, nil
}
func (f *fakeSamples) AdminListSamples(_ context.Context, _ repo.SampleFilter, _, _ int) ([]*model.Sample, int64, error) {
	var out []*model.Sample
	for _, s := range f.samples {
		if !s.Deleted {
			out = append(out, cp(s))
		}
	}
	return out, int64(len(out)), nil
}
func (f *fakeSamples) InsertSample(_ context.Context, s *model.Sample) error {
	f.id++
	s.ID = f.id
	f.samples[s.ID] = cp(s)
	return nil
}
func (f *fakeSamples) UpdateSample(_ context.Context, id int64, set map[string]any, changed bool) (bool, error) {
	s := f.samples[id]
	if s == nil || s.Deleted {
		return false, nil
	}
	s.SourceRid, s.Title = set["source_rid"].(string), set["title"].(string)
	s.Enabled = set["enabled"].(bool)
	if changed {
		s.AnalysisID, s.PendingAnalysisID = nil, nil
	}
	return true, nil
}
func (f *fakeSamples) DeleteSample(_ context.Context, id, _ int64) (bool, error) {
	s := f.samples[id]
	if s == nil || s.Deleted {
		return false, nil
	}
	s.Deleted = true
	return true, nil
}
func (f *fakeSamples) BeginSampleAnalysis(_ context.Context, id, _ int64, build func(int, string) *model.Analysis) (*model.Analysis, bool, error) {
	s := f.samples[id]
	if s == nil || s.Deleted {
		return nil, false, nil
	}
	if s.PendingAnalysisID != nil {
		if a := f.analyses[*s.PendingAnalysisID]; a != nil && (a.Status == model.AnalysisQueued || a.Status == model.AnalysisRunning) {
			return nil, true, nil
		}
	}
	a := build(s.AnalysisRound+1, s.SourceRid)
	f.id++
	a.ID = f.id
	f.analyses[a.ID] = a
	s.PendingAnalysisID, s.AnalysisRound = &a.ID, s.AnalysisRound+1
	return cp(a), false, nil
}
func (f *fakeSamples) AnalysesByIDs(_ context.Context, ids []int64) (map[int64]*model.Analysis, error) {
	out := map[int64]*model.Analysis{}
	for _, id := range ids {
		if a := f.analyses[id]; a != nil {
			out[id] = cp(a)
		}
	}
	return out, nil
}

const sampleRid = "abc123-asset-aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestSampleAdminLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeSamples()
	s := New(f, nil, nil)
	bad := []SampleReq{
		{Title: "", SourceRid: sampleRid},
		{Title: "x", SourceRid: ""},
		{Title: "x", SourceRid: "https://cdn/x.mp4"},
		{Title: "x", SourceRid: sampleRid, CoverRid: "nope"},
		{Title: "x", SourceRid: "abc123-user-aaaaaaaaaaaaaaaaaaaaaaaaaa"},                            // 用户上传类
		{Title: "x", SourceRid: sampleRid, ResultRid: "mr://abc123-user-aaaaaaaaaaaaaaaaaaaaaaaaaa"}, // 用户上传类成片
		{Title: "x", SourceRid: "abc123-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"},                            // 中间产物
		{Title: "x", SourceRid: sampleRid, Annotations: json.RawMessage(`{"dims":{"bogus":{"mode":"keep"}}}`)},
		{Title: "一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十多", SourceRid: sampleRid},
	}
	for i, req := range bad {
		if _, err := s.CreateSample(ctx, req); err == nil {
			t.Fatalf("第 %d 个应拒收", i)
		}
	}
	if _, err := s.CreateSample(ctx, SampleReq{Title: "x", SourceRid: "abc123-user-aaaaaaaaaaaaaaaaaaaaaaaaaa"}); err == nil ||
		!strings.Contains(err.Error(), "用户上传") {
		t.Fatalf("用户上传类 rid 拒收原因要可读：%v", err)
	}
	row, err := s.CreateSample(ctx, SampleReq{Title: "女装上新", Industry: "womenswear", SourceRid: "mr://" + sampleRid, ResultRid: sampleRid,
		Annotations: json.RawMessage(`{"dims":{"product":{"mode":"replace","refs":[1]}}}`), Enabled: true, Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.SourceRid != sampleRid || row.AnalysisStatus != "queued" || row.Published || row.UserVisible || row.Round != 1 || row.Annotations == nil {
		t.Fatalf("新建并预分析：%+v", row)
	}
	a := f.analyses[*f.samples[row.ID].PendingAnalysisID]
	if a.UserID != model.SystemUserID || a.SourceURL != "mr://"+sampleRid {
		t.Fatalf("系统分析行：%+v", a)
	}
	if _, err := s.AnalyzeSample(ctx, row.ID, 1); xerr.ReasonOf(err) != ReasonSampleAnalyzing {
		t.Fatalf("还在跑应 409：%v", err)
	}
	a.Status, a.Stage = model.AnalysisRunning, model.StageVision
	if got, _ := s.GetSample(ctx, row.ID); got.AnalysisStatus != "running" || got.AnalysisStage != model.StageVision {
		t.Fatalf("进行中：%+v", got)
	}
	a.Status, a.Result = model.AnalysisSucceeded, json.RawMessage(`{"durationS":12.4,"summary":"看懂了"}`)
	got, _ := s.GetSample(ctx, row.ID)
	if got.AnalysisStatus != "ready" || !got.Published || !got.UserVisible || got.Analysis == nil || got.DurationS != 12.4 || got.AnalysisNo != a.AnalysisNo {
		t.Fatalf("跑完惰性发布：%+v", got)
	}
	// 第二轮失败：已发布的保留，状态仍 ready 并带原因
	if _, err := s.AnalyzeSample(ctx, row.ID, 1); err != nil {
		t.Fatal(err)
	}
	a2 := f.analyses[*f.samples[row.ID].PendingAnalysisID]
	a2.Status, a2.ErrorMsg = model.AnalysisFailed, ptrStr("原视频太短了")
	list, _ := s.ListSamples(ctx, SampleListReq{})
	if len(list.Items) != 1 || list.Items[0].AnalysisStatus != "ready" || list.Items[0].AnalysisError != "原视频太短了" || list.Items[0].Round != 2 {
		t.Fatalf("失败不影响已发布：%+v", list.Items)
	}
	// 换原片：清掉分析
	up, err := s.UpdateSample(ctx, SampleReq{ID: row.ID, Title: "女装上新 2", SourceRid: "abc123-asset-bbbbbbbbbbbbbbbbbbbbbbbbbb", Enabled: true})
	if err != nil || up.Published || up.AnalysisStatus != "failed" {
		t.Fatalf("换原片：%+v %v", up, err)
	}
	if err := s.DeleteSample(ctx, row.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSample(ctx, row.ID); err == nil {
		t.Fatal("删除后找不到")
	}
	// 没装配示例存储：7003
	if _, err := New(nil, nil, nil).ListSamples(ctx, SampleListReq{}); err == nil {
		t.Fatal("未装配应回功能暂未开放")
	}
	_ = core.SampleRidOf
}

func ptrStr(s string) *string { return &s }
