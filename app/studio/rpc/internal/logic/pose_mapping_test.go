package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------- 提交校验

func TestPoseMappingValidate(t *testing.T) {
	base := func() *studio.SubmitPoseReq {
		return &studio.SubmitPoseReq{UserId: 1, RequestId: "r", SourceImageUrl: okURL, PoseImageUrls: urls(3), SourcePersonCount: 2,
			Mappings: []*studio.PoseMapping{{PoseIndex: 2, Mapping: []int32{1, 0}, PosePersonCount: 2}, {PoseIndex: 0, Mapping: []int32{0, -1}, PosePersonCount: 3}}}
	}
	in, err := ValidatePose(base(), nil)
	wantCode(t, "配对正常", err, 0)
	if in.SourcePersonCount != 2 || len(in.Mappings) != 2 || in.Mappings[0].PoseIndex != 0 || in.Mappings[1].PoseIndex != 2 ||
		in.Mappings[1].Mapping[0] != 1 || in.Mappings[1].Mapping[1] != 0 || in.Mappings[0].PosePersonCount != 3 {
		t.Fatalf("配对归一不对（应按 pose_index 升序）：%+v", in)
	}

	// 不传 mappings：source_person_count 忽略，旧前端不受影响
	r := base()
	r.Mappings, r.SourcePersonCount = nil, 7
	in, err = ValidatePose(r, nil)
	wantCode(t, "不传配对", err, 0)
	if in.SourcePersonCount != 0 || in.Mappings != nil {
		t.Fatalf("不传配对不应留下配对：%+v", in)
	}

	bad := []struct {
		name string
		mut  func(r *studio.SubmitPoseReq)
	}{
		{"缺原图人数", func(r *studio.SubmitPoseReq) { r.SourcePersonCount = 0 }},
		{"原图单人", func(r *studio.SubmitPoseReq) { r.SourcePersonCount = 1 }},
		{"原图超 4 人", func(r *studio.SubmitPoseReq) { r.SourcePersonCount = 5 }},
		{"长度≠原图人数", func(r *studio.SubmitPoseReq) { r.Mappings[0].Mapping = []int32{1, 0, -1} }},
		{"值越界（≥姿势图人数）", func(r *studio.SubmitPoseReq) { r.Mappings[0].Mapping = []int32{2, 0} }},
		{"值小于 -1", func(r *studio.SubmitPoseReq) { r.Mappings[0].Mapping = []int32{-2, 0} }},
		{"非负值重复", func(r *studio.SubmitPoseReq) { r.Mappings[0].Mapping = []int32{0, 0} }},
		{"全 -1", func(r *studio.SubmitPoseReq) { r.Mappings[0].Mapping = []int32{-1, -1} }},
		{"缺姿势图人数", func(r *studio.SubmitPoseReq) { r.Mappings[0].PosePersonCount = 0 }},
		{"姿势图超 4 人", func(r *studio.SubmitPoseReq) { r.Mappings[0].PosePersonCount = 5 }},
		{"姿势序号越界", func(r *studio.SubmitPoseReq) { r.Mappings[0].PoseIndex = 3 }},
		{"姿势序号为负", func(r *studio.SubmitPoseReq) { r.Mappings[0].PoseIndex = -1 }},
		{"姿势序号重复", func(r *studio.SubmitPoseReq) { r.Mappings[1].PoseIndex = 2 }},
		{"条数多于姿势图", func(r *studio.SubmitPoseReq) {
			r.PoseImageUrls = urls(1)
			r.Mappings = []*studio.PoseMapping{{PoseIndex: 0, Mapping: []int32{0, 1}, PosePersonCount: 2}, {PoseIndex: 0, Mapping: []int32{1, 0}, PosePersonCount: 2}}
		}},
	}
	for _, c := range bad {
		r := base()
		c.mut(r)
		_, err := ValidatePose(r, nil)
		wantCode(t, c.name, err, xerr.ParamError)
	}
}

// 配对只在传了时进 payload：不带配对的提交 payload（幂等哈希）与旧版一致。
func TestPoseSpecMappingPayload(t *testing.T) {
	r := &studio.SubmitPoseReq{UserId: 1, RequestId: "r", SourceImageUrl: okURL, PoseImageUrls: urls(2)}
	v, err := ValidatePose(r, nil)
	wantCode(t, "无配对", err, 0)
	sp := poseSpec(r, v)
	if _, ok := sp.Payload["mappings"]; ok {
		t.Fatal("不带配对时 payload 不应出现 mappings")
	}
	if _, ok := sp.Payload["source_person_count"]; ok {
		t.Fatal("不带配对时 payload 不应出现 source_person_count")
	}
	r.SourcePersonCount = 2
	r.Mappings = []*studio.PoseMapping{{PoseIndex: 1, Mapping: []int32{1, 0}, PosePersonCount: 2}}
	v, err = ValidatePose(r, nil)
	wantCode(t, "有配对", err, 0)
	sp = poseSpec(r, v)
	raw, _, err := payloadOf(sp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	p, err := parsePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.SourcePersonCount != 2 || p.mappingFor(1) == nil || p.mappingFor(0) != nil || p.mappingFor(1).Mapping[0] != 1 {
		t.Fatalf("payload 往返后配对不对：%+v", p)
	}
}

// ---------------------------------------------------------------- 编排：缓存键与转发

func TestPosePromptPlanCacheKey(t *testing.T) {
	sha := pipeline.SHA1Hex([]byte("pose-image"))
	p := &batchPayload{GlobalPrompt: "全局", SourcePersonCount: 2, PoseMappings: []PoseMappingInput{
		{PoseIndex: 0, Mapping: []int{1, 0}, PosePersonCount: 2},
	}}
	notes, mapping, want, keyRev, warn := p.posePromptPlan(0, sha, 2)
	if notes != "全局" || want == nil || warn != "" || len(mapping) != 2 || mapping[0] != 1 || mapping[1] != 0 {
		t.Fatalf("plan = %q %v %v %q", notes, mapping, want, warn)
	}
	// 同一张姿势图不带配对（默认配对）的键
	_, m2, w2, keyDef, _ := p.posePromptPlan(1, sha, 2)
	if m2 != nil || w2 != nil {
		t.Fatalf("没确认配对的姿势图不应带 mapping：%v", m2)
	}
	if keyRev == keyDef || !strings.HasPrefix(keyRev, keyDef) {
		t.Fatalf("反转配对必须换缓存键：rev=%s def=%s", keyRev, keyDef)
	}
	p.PoseMappings[0].Mapping = []int{0, 1}
	if _, _, _, keyFwd, _ := p.posePromptPlan(0, sha, 2); keyFwd == keyRev {
		t.Fatal("正 / 反配对缓存键不能相同")
	}

	// 本次原图识别人数与预览不同：放弃配对、退回默认，键与默认一致
	_, m3, _, key3, warn3 := p.posePromptPlan(0, sha, 3)
	if m3 != nil || key3 != keyDef || !strings.Contains(warn3, "预览 2 人") || !strings.Contains(warn3, "本次识别 3 人") {
		t.Fatalf("人数不一致应放弃配对：mapping=%v key=%s warn=%q", m3, key3, warn3)
	}
	if m, w := resolvePoseMapping(&PoseMappingInput{Mapping: []int{1, 0}}, 2, 1); m != nil || w == "" {
		t.Fatalf("原图单人不应发 mapping：%v %q", m, w)
	}
}

// fakeRev 只实现姿态两条路由；其余方法不该被调用。
type fakeRev struct {
	mu         sync.Mutex
	srcPeople  int
	posePeople int
	coerce     []int // 非空时 pose/prompt 回这个 mapping（模拟 sidecar 改写）
	failPose   map[string]error
	srcErr     error
	gotMaps    [][]int
	poseHits   int
}

func (f *fakeRev) Status(context.Context) (*reverse.Status, error) { return nil, errors.New("unused") }
func (f *fakeRev) Analyze(context.Context, reverse.AnalyzeReq) (*reverse.AnalyzeResp, error) {
	return nil, errors.New("unused")
}
func (f *fakeRev) AnalyzeBatch(context.Context, reverse.AnalyzeBatchReq) (*reverse.AnalyzeBatchResp, error) {
	return nil, errors.New("unused")
}
func (f *fakeRev) CropPlan(context.Context, reverse.CropPlanReq) (*reverse.CropPlanResp, error) {
	return nil, errors.New("unused")
}
func (f *fakeRev) CropCheck(context.Context, []reverse.CropCheckItem) ([]reverse.CropCheckResult, error) {
	return nil, errors.New("unused")
}

func people(n int) []reverse.PoseSourcePerson {
	out := make([]reverse.PoseSourcePerson, n)
	for i := range out {
		out[i] = reverse.PoseSourcePerson{Label: "P" + string(rune('1'+i)), Position: "画面左侧", Primary: i == 0, GenderAge: "成年女性", Outfit: []string{"西装", "短裤"}}
	}
	return out
}

func (f *fakeRev) PoseSource(_ context.Context, data string, _ ...string) (*reverse.PoseSource, error) {
	if f.srcErr != nil {
		return nil, f.srcErr
	}
	n := f.srcPeople
	return &reverse.PoseSource{Source: reverse.PoseSourceData{HasPerson: true, PersonCount: n, People: people(n), Interaction: "无"}}, nil
}

func (f *fakeRev) PosePrompt(_ context.Context, src *reverse.PoseSource, data, notes string, mapping []int) (*reverse.PosePrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poseHits++
	f.gotMaps = append(f.gotMaps, mapping)
	if err := f.failPose[data]; err != nil {
		return nil, err
	}
	pp := &reverse.PosePrompt{Prompt: "p"}
	for i := 0; i < f.posePeople; i++ {
		pp.Pose.People = append(pp.Pose.People, reverse.PosePerson{Label: "P" + string(rune('1'+i)), Position: "画面右侧"})
	}
	pp.Pose.PersonCount = f.posePeople
	n := src.Source.PeopleCount()
	switch {
	case n < 2:
	case f.coerce != nil:
		pp.Mapping = f.coerce
	case mapping != nil:
		pp.Mapping = mapping
	default:
		for i := 0; i < n; i++ {
			pp.Mapping = append(pp.Mapping, i)
		}
	}
	return pp, nil
}

func TestFetchPosePromptForwardsMapping(t *testing.T) {
	rev := &fakeRev{srcPeople: 2, posePeople: 2}
	src, _ := rev.PoseSource(context.Background(), "x")
	want := &PoseMappingInput{PoseIndex: 0, Mapping: []int{1, 0}, PosePersonCount: 2}
	pp, warn, err := fetchPosePrompt(context.Background(), rev, src, "data:image/jpeg;base64,AA", "n", []int{1, 0}, want)
	if err != nil || warn != "" {
		t.Fatalf("err=%v warn=%q", err, warn)
	}
	if len(rev.gotMaps) != 1 || len(rev.gotMaps[0]) != 2 || rev.gotMaps[0][0] != 1 || rev.gotMaps[0][1] != 0 {
		t.Fatalf("mapping 没有转发给 sidecar：%v", rev.gotMaps)
	}
	if pp.Mapping[0] != 1 {
		t.Fatalf("mapping = %v", pp.Mapping)
	}

	// 姿势图人数与预览不同 → 丢掉带配对的结果，不带配对重读一次（默认配对）并告警
	rev.posePeople = 3
	hits := rev.poseHits
	pp, warn, err = fetchPosePrompt(context.Background(), rev, src, "d", "", []int{1, 0}, want)
	if err != nil || !strings.Contains(warn, "姿势图人数与预览不一致") || !strings.Contains(warn, "已按默认配对生成") {
		t.Fatalf("err=%v warn=%q", err, warn)
	}
	if rev.poseHits != hits+2 || rev.gotMaps[len(rev.gotMaps)-1] != nil || pp.Mapping[0] != 0 || pp.Mapping[1] != 1 {
		t.Fatalf("人数不一致应退回默认配对重读：hits=%d maps=%v mapping=%v", rev.poseHits-hits, rev.gotMaps, pp.Mapping)
	}
	// 重读失败 → 返回错误（不按错位的配对生成）
	failOnSecond := &secondCallFails{fakeRev: rev}
	if _, _, err = fetchPosePrompt(context.Background(), failOnSecond, src, "d", "", []int{1, 0}, want); err == nil {
		t.Fatal("默认配对重读失败应返回错误")
	}
	// sidecar 改写配对 → 告警
	rev.posePeople, rev.coerce = 2, []int{0, 1}
	if _, warn, _ = fetchPosePrompt(context.Background(), rev, src, "d", "", []int{1, 0}, want); !strings.Contains(warn, "校正") {
		t.Fatalf("warn = %q", warn)
	}
	// 没确认配对：不发 mapping、不告警
	rev.coerce = nil
	if _, warn, _ = fetchPosePrompt(context.Background(), rev, src, "d", "", nil, nil); warn != "" || rev.gotMaps[len(rev.gotMaps)-1] != nil {
		t.Fatalf("默认配对不应带 mapping：%v %q", rev.gotMaps, warn)
	}
}

// secondCallFails 第二次 pose/prompt 起报错（模拟默认配对重读失败）。
type secondCallFails struct {
	*fakeRev
	n int
}

func (f *secondCallFails) PosePrompt(ctx context.Context, src *reverse.PoseSource, data, notes string, mapping []int) (*reverse.PosePrompt, error) {
	f.n++
	if f.n >= 2 {
		return nil, &reverse.Error{Path: reverse.PathPosePrompt, Status: 502, Kind: reverse.KindUpstream, Message: "bad gateway"}
	}
	return f.fakeRev.PosePrompt(ctx, src, data, notes, mapping)
}

// 缓存命中也要带回配对告警：新缓存原样带回当时的结论，旧缓存按结果重算。
func TestPosePromptCacheMappingWarn(t *testing.T) {
	want := &PoseMappingInput{PoseIndex: 0, Mapping: []int{1, 0}, PosePersonCount: 2}
	pp := reverse.PosePrompt{Prompt: "p", Mapping: []int{0, 1}}
	pp.Pose.People = []reverse.PosePerson{{Label: "P1"}, {Label: "P2"}, {Label: "P3"}}

	// 新缓存：存了结论（默认配对重读后的告警），往返后原样返回，不按结果重算
	stored := posePromptCache{PosePrompt: pp, MappingChecked: true, MappingWarn: "姿势图人数与预览不一致（预览 2 人、本次识别 3 人），已按默认配对生成（实际配对 [0 1]）"}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var got posePromptCache
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "p" || len(got.Mapping) != 2 || !got.MappingChecked || got.mappingWarn(want, []int{1, 0}) != stored.MappingWarn {
		t.Fatalf("新缓存往返不对：%+v", got)
	}
	// 新缓存且无告警：不重算出告警
	clean := posePromptCache{PosePrompt: pp, MappingChecked: true}
	if w := clean.mappingWarn(want, []int{1, 0}); w != "" {
		t.Fatalf("已核对过且无告警的缓存不应重算：%q", w)
	}

	// 旧缓存（只有 sidecar 返回）：按结果重算
	legacy, _ := json.Marshal(pp)
	var old posePromptCache
	if err := json.Unmarshal(legacy, &old); err != nil {
		t.Fatal(err)
	}
	if old.MappingChecked || !strings.Contains(old.mappingWarn(want, []int{1, 0}), "姿势图人数与预览不一致") {
		t.Fatalf("旧缓存应按结果重算告警：%+v", old)
	}
	old.Pose.People = old.Pose.People[:2]
	if w := old.mappingWarn(want, []int{1, 0}); !strings.Contains(w, "校正") {
		t.Fatalf("旧缓存配对被改写应告警：%q", w)
	}
	if w := old.mappingWarn(nil, nil); w != "" {
		t.Fatalf("没确认配对不告警：%q", w)
	}
}

// ---------------------------------------------------------------- 配对预览

type previewMedia struct{ fail map[string]bool }

func (m *previewMedia) IngestURL(_ context.Context, u, _, _ string) (mediasvc.Resource, error) {
	if m.fail[u] {
		return mediasvc.Resource{}, &mediasvc.Error{Status: 404, Message: "not found"}
	}
	return mediasvc.Resource{RID: u, Width: 100, Height: 100}, nil
}
func (m *previewMedia) Conform(_ context.Context, rid string, _ mediasvc.ConformParams, _ string) (mediasvc.Resource, error) {
	return mediasvc.Resource{RID: rid, Width: 100, Height: 100}, nil
}
func (m *previewMedia) Crop(context.Context, string, []mediasvc.PixelBox, mediasvc.CropParams, string) ([]mediasvc.Resource, error) {
	return nil, errors.New("unused")
}
func (m *previewMedia) Grid(context.Context, []string, mediasvc.GridParams, string) (mediasvc.Resource, error) {
	return mediasvc.Resource{}, errors.New("unused")
}
func (m *previewMedia) FetchBytes(_ context.Context, rid string) ([]byte, string, error) {
	return []byte(rid), "image/jpeg", nil
}
func (m *previewMedia) ResourceURLs(context.Context, []string, time.Duration) (map[string]string, error) {
	return nil, errors.New("unused")
}
func (m *previewMedia) CopyToLibrary(context.Context, string, string, string) (mediasvc.Resource, error) {
	return mediasvc.Resource{}, errors.New("unused")
}

func newPreviewer(rev *fakeRev, media *previewMedia) *posePreviewer {
	return &posePreviewer{media: media, rev: rev, owns: func(u string) bool { return strings.HasPrefix(u, "https://oss.example.com/") }}
}

func TestPreviewPoseMultiPerson(t *testing.T) {
	rev := &fakeRev{srcPeople: 2, posePeople: 2, failPose: map[string]error{}}
	poses := urls(3)
	bad := reverse.DataURL("image/jpeg", []byte(poses[1]))
	rev.failPose[bad] = &reverse.Error{Path: reverse.PathPosePrompt, Status: 422, Kind: reverse.KindInvalid, Message: "姿势参考图中没有识别到人物，请换一张"}
	out, err := newPreviewer(rev, &previewMedia{}).preview(context.Background(), &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: poses})
	if err != nil {
		t.Fatal(err)
	}
	if !out.MappingEnabled || out.Source.PersonCount != 2 || len(out.Source.People) != 2 || out.Source.People[0].Summary != "西装；短裤" {
		t.Fatalf("source = %+v", out.Source)
	}
	if len(out.Poses) != 3 || rev.poseHits != 3 {
		t.Fatalf("每张姿势图一条：%d / hits %d", len(out.Poses), rev.poseHits)
	}
	for i, it := range out.Poses {
		if int(it.PoseIndex) != i {
			t.Fatalf("顺序不对：%+v", it)
		}
		if i == 1 {
			if it.Error == "" || it.Pose != nil || !strings.Contains(it.Error, "没有识别到人物") {
				t.Fatalf("失败的姿势图应只写 error：%+v", it)
			}
			continue
		}
		if it.Error != "" || it.Pose.PersonCount != 2 || len(it.DefaultMapping) != 2 || it.DefaultMapping[0] != 0 || it.DefaultMapping[1] != 1 {
			t.Fatalf("姿势图 %d = %+v", i, it)
		}
	}
	for _, m := range rev.gotMaps {
		if m != nil {
			t.Fatalf("预览只取默认配对，不应带 mapping：%v", rev.gotMaps)
		}
	}
}

// 原图单人：不需要配对（mapping_enabled=false、default_mapping 为空），但每张姿势图仍回人数，供前端提示多出的人物会被忽略。
func TestPreviewPoseSinglePersonReportsPoseCounts(t *testing.T) {
	rev := &fakeRev{srcPeople: 1, posePeople: 2}
	out, err := newPreviewer(rev, &previewMedia{}).preview(context.Background(), &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(2)})
	if err != nil {
		t.Fatal(err)
	}
	if out.MappingEnabled || out.Source.PersonCount != 1 || len(out.Poses) != 2 || rev.poseHits != 2 {
		t.Fatalf("单人也应逐张读姿势图：%+v hits=%d", out, rev.poseHits)
	}
	for i, it := range out.Poses {
		if int(it.PoseIndex) != i || it.Error != "" || it.Pose == nil || it.Pose.PersonCount != 2 || len(it.DefaultMapping) != 0 {
			t.Fatalf("姿势图 %d = %+v", i, it)
		}
	}
}

func TestPreviewPoseErrors(t *testing.T) {
	ctx := context.Background()
	ok := &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(1)}

	_, err := newPreviewer(&fakeRev{srcPeople: 2}, &previewMedia{}).preview(ctx, &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: "https://evil.example.com/a.jpg", PoseImageUrls: urls(1)})
	wantCode(t, "非本服务地址", err, xerr.ParamError)
	_, err = newPreviewer(&fakeRev{srcPeople: 2}, &previewMedia{}).preview(ctx, &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(MaxPoses + 1)})
	wantCode(t, "姿势超限", err, xerr.ParamError)
	_, err = newPreviewer(&fakeRev{srcPeople: 2}, &previewMedia{}).preview(ctx, &studio.PreviewPoseReq{SourceImageUrl: okURL, PoseImageUrls: urls(1)})
	wantCode(t, "缺用户", err, xerr.ParamMissing)

	pv := newPreviewer(&fakeRev{srcPeople: 2}, &previewMedia{})
	pv.media = nil
	_, err = pv.preview(ctx, ok)
	wantCode(t, "媒体服务未配置", err, xerr.NotReady)

	rev := &fakeRev{srcErr: &reverse.Error{Path: reverse.PathPoseSource, Status: 422, Kind: reverse.KindInvalid, Message: "暂不支持未成年人图片，请上传成年模特的原图"}}
	_, err = newPreviewer(rev, &previewMedia{}).preview(ctx, ok)
	wantCode(t, "原图被拒", err, xerr.ParamError)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "未成年") {
		t.Fatalf("应透出 sidecar 文案：%q", msg)
	}

	// 同一用户并发预览：第二个直接拒绝
	previewInflight.Store(int64(9), struct{}{})
	_, err = newPreviewer(&fakeRev{srcPeople: 2}, &previewMedia{}).preview(ctx, ok)
	previewInflight.Delete(int64(9))
	wantCode(t, "并发预览", err, xerr.Conflict)
}

// slowRev pose/prompt 耗时 delay（尊重 ctx），并统计同时在途的最大数。
type slowRev struct {
	*fakeRev
	delay    time.Duration
	mu       sync.Mutex
	inflight int
	peak     int
	calls    int
}

func (f *slowRev) PosePrompt(ctx context.Context, src *reverse.PoseSource, data, notes string, mapping []int) (*reverse.PosePrompt, error) {
	f.mu.Lock()
	f.inflight++
	f.calls++
	f.peak = max(f.peak, f.inflight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, &reverse.Error{Path: reverse.PathPosePrompt, Kind: reverse.KindTransport, Message: ctx.Err().Error(), Err: ctx.Err()}
	}
	return f.fakeRev.PosePrompt(ctx, src, data, notes, mapping)
}

// 预览受全进程名额约束：同时在途的 sidecar 调用不超过 cap(slots)，与本次几张姿势图无关。
func TestPreviewPoseRespectsSlots(t *testing.T) {
	rev := &slowRev{fakeRev: &fakeRev{srcPeople: 2, posePeople: 2}, delay: 30 * time.Millisecond}
	pv := newPreviewer(nil, &previewMedia{})
	pv.rev, pv.slots = rev, make(chan struct{}, 2)
	out, err := pv.preview(context.Background(), &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(6)})
	if err != nil {
		t.Fatal(err)
	}
	if rev.peak > 2 || rev.calls != 6 {
		t.Fatalf("同时在途 %d（上限 2），调用 %d 次", rev.peak, rev.calls)
	}
	for _, it := range out.Poses {
		if it.Error != "" || it.Pose == nil {
			t.Fatalf("姿势图 %d 应成功：%+v", it.PoseIndex, it)
		}
	}
	if len(pv.slots) != 0 {
		t.Fatalf("名额没有归还：%d", len(pv.slots))
	}
}

// 时间不够时：已读完的照常返回，靠后的几张记「识别超时」，整个预览不报错。
func TestPreviewPosePartialOnDeadline(t *testing.T) {
	oldSafety, oldMin := previewSafety, previewMinCall
	previewSafety, previewMinCall = 0, 80*time.Millisecond
	defer func() { previewSafety, previewMinCall = oldSafety, oldMin }()

	rev := &slowRev{fakeRev: &fakeRev{srcPeople: 2, posePeople: 2}, delay: 100 * time.Millisecond}
	pv := newPreviewer(nil, &previewMedia{})
	pv.rev, pv.slots = rev, make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	out, err := pv.preview(ctx, &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(5)})
	if err != nil {
		t.Fatalf("超时应返回部分结果而不是报错：%v", err)
	}
	if len(out.Poses) != 5 || out.Poses[0].Error != "" || out.Poses[0].Pose == nil {
		t.Fatalf("第一张应读完：%+v", out.Poses)
	}
	last := out.Poses[4]
	if last.Pose != nil || !strings.Contains(last.Error, "识别超时") {
		t.Fatalf("最后一张应记识别超时：%+v", last)
	}
	if rev.calls > 3 {
		t.Fatalf("剩余时间不够一次读图时不应再发：调用 %d 次", rev.calls)
	}
}

// fakePutter 记录预览写入的原图读图结果。
type fakePutter struct {
	batchID   int64
	key, kind string
	payload   json.RawMessage
	err       error
}

func (f *fakePutter) Put(_ context.Context, batchID int64, key, kind string, payload json.RawMessage) error {
	f.batchID, f.key, f.kind, f.payload = batchID, key, kind, payload
	return f.err
}

// 预览把原图读图结果按 (用户, 原图 sha1) 存起来，供编排复用；写失败不影响预览。
func TestPreviewPoseSavesSource(t *testing.T) {
	put := &fakePutter{}
	pv := newPreviewer(&fakeRev{srcPeople: 2, posePeople: 2}, &previewMedia{})
	pv.cache = put
	pv.now = func() time.Time { return time.Unix(1700000000, 0) }
	if _, err := pv.preview(context.Background(), &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(1)}); err != nil {
		t.Fatal(err)
	}
	// previewMedia.FetchBytes 回 rid（= URL）作字节，原图 sha1 = sha1(okURL)
	if put.batchID != -9 || put.key != pipeline.SourceKey(pipeline.SHA1Hex([]byte(okURL))) || put.kind != model.CacheSource {
		t.Fatalf("写入坐标不对：batch=%d key=%s kind=%s", put.batchID, put.key, put.kind)
	}
	var c previewSourceCache
	if err := json.Unmarshal(put.payload, &c); err != nil || c.CachedAt != 1700000000 || len(c.Source) == 0 {
		t.Fatalf("写入内容不对：%s", put.payload)
	}
	var src reverse.PoseSourceData
	if err := json.Unmarshal(c.Source, &src); err != nil || src.PeopleCount() != 2 {
		t.Fatalf("source 不对：%s", c.Source)
	}

	put.err = errors.New("db down")
	if _, err := pv.preview(context.Background(), &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(1)}); err != nil {
		t.Fatalf("写缓存失败不应影响预览：%v", err)
	}
}

// ---------------------------------------------------------------- 编排复用预览的原图读图结果

func previewSourceRun(t *testing.T, srcCount int) (*batchRun, sqlmock.Sqlmock, *time.Time) {
	t.Helper()
	f, mock, _ := newTestFlow(t, onBill())
	now := time.Unix(1700000000, 0)
	f.now = func() time.Time { return now }
	return &batchRun{f: f, b: &model.Batch{ID: 9, UserID: 7, Kind: model.KindPose}, p: &batchPayload{SourcePersonCount: srcCount}}, mock, &now
}

func TestBatchPreviewSource(t *testing.T) {
	q := regexp.QuoteMeta("WHERE (pc.batch_id = -7) AND (pc.cache_key = 'abc') AND (pc.kind = 'source') LIMIT 1")
	body := func(n int, at int64) string {
		raw, _ := json.Marshal(previewSourceCache{Source: json.RawMessage(`{"has_person":true,"person_count":` + fmt.Sprint(n) + `,"people":[` +
			strings.TrimSuffix(strings.Repeat(`{"label":"P","position":"画面左侧"},`, n), ",") + `]}`), CachedAt: at})
		return string(raw)
	}
	rows := func(payload string) *sqlmock.Rows { return sqlmock.NewRows([]string{"payload"}).AddRow(payload) }

	// 命中且未过期、人数与确认的一致 → 复用
	r, mock, now := previewSourceRun(t, 2)
	mock.ExpectQuery(q).WillReturnRows(rows(body(2, now.Unix()-600)))
	s := r.previewSource(context.Background(), "ABC")
	if s == nil || s.Source.PeopleCount() != 2 || len(s.Raw) == 0 {
		t.Fatalf("应复用预览结果：%+v", s)
	}
	// 过期 → 不复用
	mock.ExpectQuery(q).WillReturnRows(rows(body(2, now.Unix()-int64(previewSourceTTL/time.Second)-1)))
	if s := r.previewSource(context.Background(), "abc"); s != nil {
		t.Fatal("过期的预览结果不应复用")
	}
	// 人数与提交时确认的不一致 → 不复用
	mock.ExpectQuery(q).WillReturnRows(rows(body(3, now.Unix())))
	if s := r.previewSource(context.Background(), "abc"); s != nil {
		t.Fatal("人数不一致的预览结果不应复用")
	}
	// 没有 / 读库失败 → 不复用
	mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"payload"}))
	if s := r.previewSource(context.Background(), "abc"); s != nil {
		t.Fatal("没有预览结果")
	}
	mock.ExpectQuery(q).WillReturnError(errors.New("db down"))
	if s := r.previewSource(context.Background(), "abc"); s != nil {
		t.Fatal("读库失败当没有")
	}
	// 没确认配对（source_person_count=0）：人数不校验，照样复用
	r.p.SourcePersonCount = 0
	mock.ExpectQuery(q).WillReturnRows(rows(body(3, now.Unix())))
	if s := r.previewSource(context.Background(), "abc"); s == nil {
		t.Fatal("没确认配对时也应复用同图的预览结果")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- 商拍提示词版本落批次

func TestTryonPromptVersionPersisted(t *testing.T) {
	qGet := regexp.QuoteMeta("WHERE (pc.batch_id = 9) AND (pc.cache_key = 'prompt-version') AND (pc.kind = 'analyze') LIMIT 1")
	qPut := regexp.QuoteMeta("INSERT INTO `afv_studio_prompt_cache`")
	newRun := func(t *testing.T, pct int) (*batchRun, sqlmock.Sqlmock) {
		f, mock, _ := newTestFlow(t, onBill())
		f.cfg.TryonPrompt.NewPercent = pct
		return &batchRun{f: f, b: &model.Batch{ID: 9, UserID: 7, Kind: model.KindTryon}, p: &batchPayload{Model: "Seedream"}}, mock
	}

	// 首次：按当前比例（100% → new）决定并先落库
	r, mock := newRun(t, 100)
	mock.ExpectQuery(qGet).WillReturnRows(sqlmock.NewRows([]string{"payload"}))
	mock.ExpectExec(qPut + ".*" + regexp.QuoteMeta(`'prompt-version', 'analyze', '{"version":"new","target":"seedream","decided":true}'`)).WillReturnResult(ok1)
	pv, pt, err := r.tryonPromptVersion(context.Background())
	if err != nil || pv != pipeline.PromptVersionNew || pt != pipeline.TargetSeedream {
		t.Fatalf("首次应按比例决定：%q %q %v", pv, pt, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// 重放：比例已改成 0，仍沿用落库的 new，不再写
	r, mock = newRun(t, 0)
	mock.ExpectQuery(qGet).WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(`{"version":"new","target":"seedream","decided":true}`))
	if pv, pt, err = r.tryonPromptVersion(context.Background()); err != nil || pv != "new" || pt != "seedream" {
		t.Fatalf("重放应沿用首次版本：%q %q %v", pv, pt, err)
	}
	// 首次决定为稳定版（空串）也要沿用：比例改成 100 不换版本
	r, mock = newRun(t, 100)
	mock.ExpectQuery(qGet).WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(`{"version":"","target":"","decided":true}`))
	if pv, pt, err = r.tryonPromptVersion(context.Background()); err != nil || pv != "" || pt != "" {
		t.Fatalf("稳定版也应沿用：%q %q %v", pv, pt, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// 落库失败：可重试错误，不按没落库的版本继续
	r, mock = newRun(t, 100)
	mock.ExpectQuery(qGet).WillReturnRows(sqlmock.NewRows([]string{"payload"}))
	mock.ExpectExec(qPut).WillReturnError(errors.New("db down"))
	_, _, err = r.tryonPromptVersion(context.Background())
	var se *stepError
	if !errors.As(err, &se) || !se.Retryable {
		t.Fatalf("落库失败应为可重试错误：%v", err)
	}
}

// 反推缓存带版本：往返保留；旧缓存（无 versioned）能解析。
func TestAnalyzeCacheVersion(t *testing.T) {
	raw, _ := json.Marshal(&analyzeCache{AnalyzeResp: reverse.AnalyzeResp{Prompt: "p"}, PromptVersion: "new", TargetModel: "seedream", Versioned: true})
	var c analyzeCache
	if err := json.Unmarshal(raw, &c); err != nil || c.Prompt != "p" || c.PromptVersion != "new" || !c.Versioned {
		t.Fatalf("往返不对：%s", raw)
	}
	var old analyzeCache
	if err := json.Unmarshal([]byte(`{"prompt":"q","meta":{"model":"m"},"warning":""}`), &old); err != nil || old.Prompt != "q" || old.Versioned || old.Meta.Model != "m" {
		t.Fatalf("旧缓存解析不对：%+v", old)
	}
}

// ---------------------------------------------------------------- 提示词灰度运行时开关

func TestIntKnob(t *testing.T) {
	var (
		val   = "30"
		has   = true
		fail  error
		loads int
	)
	now := time.Unix(1000, 0)
	k := newIntKnob(KnobTryonNewPromptPercent, 5, 0, 100, func(context.Context, string) (string, bool, error) {
		loads++
		return val, has, fail
	})
	k.now = func() time.Time { return now }
	ctx := context.Background()
	if v := k.Get(ctx); v != 30 {
		t.Fatalf("库里有值应覆盖 yaml：%d", v)
	}
	val = "80"
	if v := k.Get(ctx); v != 30 || loads != 1 {
		t.Fatalf("TTL 内应走缓存：%d loads=%d", v, loads)
	}
	now = now.Add(knobTTL)
	if v := k.Get(ctx); v != 80 {
		t.Fatalf("过期后应重读：%d", v)
	}
	now = now.Add(knobTTL)
	fail = errors.New("db down")
	if v := k.Get(ctx); v != 80 {
		t.Fatalf("查库失败沿用上次：%d", v)
	}
	now, fail, val = now.Add(knobTTL), nil, "abc"
	if v := k.Get(ctx); v != 5 {
		t.Fatalf("非法值回落 yaml：%d", v)
	}
	now, val = now.Add(knobTTL), "101"
	if v := k.Get(ctx); v != 5 {
		t.Fatalf("越界回落 yaml：%d", v)
	}
	now, has = now.Add(knobTTL), false
	if v := k.Get(ctx); v != 5 {
		t.Fatalf("没有该行用 yaml：%d", v)
	}
	var nilKnob *intKnob
	if nilKnob.Get(ctx) != 0 {
		t.Fatal("nil knob")
	}
	f := &flow{}
	f.cfg.TryonPrompt.NewPercent = 12
	if f.tryonNewPromptPercent(ctx) != 12 {
		t.Fatal("没有运行时开关时用 yaml")
	}
}
