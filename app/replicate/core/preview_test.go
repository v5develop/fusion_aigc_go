package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"
)

// ---- 定妆图存储替身（语义同 repo/preview.go：占坑唯一、额度原子「有余量才 +1」）----

type memPreviewStore struct {
	mu     sync.Mutex
	now    func() time.Time
	id     int64
	rows   map[string]*model.Preview // user|hash
	quota  map[string]int            // user|day
	failOn string                    // 非空：对应方法报错（acquire / claim）
}

func newMemPreviewStore(now func() time.Time) *memPreviewStore {
	return &memPreviewStore{now: now, rows: map[string]*model.Preview{}, quota: map[string]int{}}
}

func pk(u int64, k string) string { return fmt.Sprintf("%d|%s", u, k) }

func (m *memPreviewStore) GetPreview(_ context.Context, u int64, h string) (*model.Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[pk(u, h)]; r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}

func (m *memPreviewStore) ClaimPreview(_ context.Context, p *model.Preview, stale time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "claim" {
		return false, errors.New("库故障")
	}
	k := pk(p.UserID, p.PlanHash)
	if cur := m.rows[k]; cur != nil {
		if cur.Status != model.PreviewGenerating || m.now().Sub(cur.UpdatedAt) < stale {
			return false, nil
		}
		cur.UpdatedAt, p.ID = m.now(), cur.ID
		return true, nil
	}
	m.id++
	c := *p
	c.ID, c.Status, c.UpdatedAt = m.id, model.PreviewGenerating, m.now()
	m.rows[k], p.ID = &c, c.ID
	return true, nil
}

func (m *memPreviewStore) FinishPreview(_ context.Context, id int64, rid, call string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id && r.Status == model.PreviewGenerating {
			r.Status, r.ImageRid, r.CallID = model.PreviewDone, rid, call
			return true, nil
		}
	}
	return false, nil
}

func (m *memPreviewStore) DeletePreviewClaim(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.rows {
		if r.ID == id && r.Status == model.PreviewGenerating {
			delete(m.rows, k)
		}
	}
	return nil
}

func (m *memPreviewStore) AcquirePreviewQuota(_ context.Context, u int64, day string, total int) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "acquire" {
		return 0, false, errors.New("库故障")
	}
	k := pk(u, day)
	if m.quota[k] >= total {
		return m.quota[k], false, nil
	}
	m.quota[k]++
	return m.quota[k], true, nil
}

func (m *memPreviewStore) ReleasePreviewQuota(_ context.Context, u int64, day string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k := pk(u, day); m.quota[k] > 0 {
		m.quota[k]--
	}
	return nil
}

func (m *memPreviewStore) PreviewQuotaUsed(_ context.Context, u int64, day string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quota[pk(u, day)], nil
}

// ---- 场景 ----

var previewImageRid = workRid("preview-image")

type previewEnv struct {
	*env
	ps *memPreviewStore
}

// newPreviewEnv 一条 12 秒的分析（带 3 张均匀帧）+ 定妆图存储 + 出图结果。
func newPreviewEnv(t *testing.T) (*previewEnv, *PlanView) {
	e := newEnv(t, false)
	ps := newMemPreviewStore(func() time.Time { return e.clock })
	WithPreview(ps)(e.svc)
	e.gw.analysis["image.edit"] = json.RawMessage(fmt.Sprintf(`{"operation":"image.edit","outputs":[{"index":0,"status":"SUCCEEDED","media":{"rid":"%s"}}]}`, previewImageRid))
	a := e.seedAnalysis(12)
	st, _ := json.Marshal(AnalysisState{Frames: []FrameOut{{T: 0, Rid: testRid("fa")}, {T: 6, Rid: testRid("fb")}, {T: 11, Rid: testRid("fc")}}})
	for _, x := range e.store.analyses {
		if x.ID == a.ID {
			x.State = st
		}
	}
	return &previewEnv{e, ps}, e.plan(a)
}

func errReason(err error) string { return xerr.ReasonOf(err) }

func TestPreviewSuccess(t *testing.T) {
	e, pv := newPreviewEnv(t)
	v, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if err != nil {
		t.Fatal(err)
	}
	if v.PlanNo != pv.PlanNo || v.ImageRid != previewImageRid || v.QuotaUsed != 1 || v.QuotaTotal != 3 || v.Replay {
		t.Fatalf("视图不对：%+v", v)
	}
	if !strings.HasPrefix(v.ImageURL, "https://cdn.test/") || v.URLExpiresAt == nil || v.FrameURL == "" {
		t.Fatalf("应现签短时地址：%+v", v)
	}
	if v.FrameAtSec != 6 {
		t.Fatalf("应取离第 1 段中点最近的帧（6s）：%v", v.FrameAtSec)
	}
	// 请求形状：image.edit、图 1 = 原片帧、图 2 起 = 方案参考图（顺序）、n=1、提示词按图号写改法
	if len(e.gw.reqs) != 1 {
		t.Fatalf("应只提交一次：%d", len(e.gw.reqs))
	}
	for _, req := range e.gw.reqs {
		if req.Operation != "image.edit" || req.ModelGroup != "img.gpt-image-2.5-sunburst" || req.MediaPurpose != aigateway.MediaPurposeIntermediate {
			t.Fatalf("请求头不对：%+v", req)
		}
		raw, _ := json.Marshal(req.Input)
		var in struct {
			Prompt string `json:"prompt"`
			Images []struct {
				URI string `json:"uri"`
			} `json:"images"`
			N int `json:"n"`
		}
		_ = json.Unmarshal(raw, &in)
		if len(in.Images) != 2 || in.Images[0].URI != "mr://"+testRid("fb") || in.Images[1].URI != testRef || in.N != 1 {
			t.Fatalf("图序不对：%s", raw)
		}
		for _, sub := range []string{"图1", "商品", "图2", "参考图1", "6.0 秒", "保持不变"} {
			if !strings.Contains(in.Prompt, sub) {
				t.Errorf("提示词缺 %q：%s", sub, in.Prompt)
			}
		}
	}
}

func TestPreviewReplayNoQuota(t *testing.T) {
	e, pv := newPreviewEnv(t)
	first, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replay || second.ImageRid != first.ImageRid || second.QuotaUsed != 1 || len(e.gw.submits) != 1 {
		t.Fatalf("重复调用应回放、不占额度、不再出图：%+v submits=%d", second, len(e.gw.submits))
	}
	// 额度用完后，已出过图的方案仍可回放
	e.ps.quota[pk(uid, e.svc.previewDay())] = 3
	third, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if err != nil || !third.Replay {
		t.Fatalf("额度用完后已有的图仍应回放：%v %+v", err, third)
	}
}

func TestPreviewQuotaExhausted(t *testing.T) {
	e, pv := newPreviewEnv(t)
	e.ps.quota[pk(uid, e.svc.previewDay())] = 3
	_, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if errReason(err) != ReasonPreviewQuotaExhausted {
		t.Fatalf("应 PREVIEW_QUOTA_EXHAUSTED：%v", err)
	}
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "明天") || strings.Contains(msg, "PREVIEW_") {
		t.Fatalf("文案应友好并说明明天恢复：%q", msg)
	}
	if len(e.gw.submits) != 0 {
		t.Fatal("额度用完不得出图")
	}
	// 自然日按 Asia/Shanghai 翻篇：次日恢复
	e.clock = e.clock.Add(24 * time.Hour)
	if v, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo); err != nil || v.QuotaUsed != 1 {
		t.Fatalf("次日应恢复：%v %+v", err, v)
	}
}

func TestPreviewDayIsShanghai(t *testing.T) {
	e, _ := newPreviewEnv(t)
	e.clock = time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC) // 北京 11-14 01:00
	if d := e.svc.previewDay(); d != "2026-11-14" {
		t.Fatalf("自然日应按上海时区：%s", d)
	}
}

func TestPreviewConcurrentQuota(t *testing.T) {
	e, _ := newPreviewEnv(t)
	a := e.seedAnalysis(12)
	var plans []string
	for i := 0; i < 8; i++ { // 8 张不同方案（不同指纹）并发抢 3 次额度
		pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
			Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "scene": {Mode: ModeDescribe, Note: fmt.Sprintf("场景%d", i)}}}})
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, pv.PlanNo)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, exhausted := 0, 0
	for _, no := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.PreviewFrame(e.ctx, uid, no)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errReason(err) == ReasonPreviewQuotaExhausted:
				exhausted++
			default:
				t.Errorf("意外错误：%v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 3 || exhausted != 5 || e.ps.quota[pk(uid, e.svc.previewDay())] != 3 || len(e.gw.submits) != 3 {
		t.Fatalf("并发下只能成功 3 张：ok=%d exhausted=%d used=%d submits=%d", ok, exhausted, e.ps.quota[pk(uid, e.svc.previewDay())], len(e.gw.submits))
	}
}

func TestPreviewGatewayFailureKeepsQuota(t *testing.T) {
	e, pv := newPreviewEnv(t)
	e.gw.submitErr = &aigateway.Error{HTTPStatus: 503, Code: "unavailable"}
	_, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if errReason(err) != reasonThirdParty {
		t.Fatalf("网关失败应是可重试的第三方错误：%v", err)
	}
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "没有占用") {
		t.Fatalf("应告诉用户没占次数：%q", msg)
	}
	if e.ps.quota[pk(uid, e.svc.previewDay())] != 0 || len(e.ps.rows) != 0 {
		t.Fatalf("失败不占额度、不留占坑：quota=%v rows=%d", e.ps.quota, len(e.ps.rows))
	}
	// 网关恢复后重试成功，只占 1 次
	e.gw.submitErr = nil
	if v, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo); err != nil || v.QuotaUsed != 1 {
		t.Fatalf("重试应成功：%v %+v", err, v)
	}
}

// 网关调用终态为 FAILED / 没有产物 / 等不到：都不占额度。
func TestPreviewFailedCallAndTimeout(t *testing.T) {
	cases := map[string]Gateway{
		"failed":    &stubGW{call: &aigateway.Call{ID: "c1", Status: aigateway.StatusFailed, Failure: &aigateway.Failure{Class: "provider_error"}}},
		"no-output": &stubGW{call: &aigateway.Call{ID: "c2", Status: aigateway.StatusSucceeded, Result: &aigateway.Result{Operation: "image.edit"}}},
		"timeout":   &stubGW{call: &aigateway.Call{ID: "c3", Status: aigateway.StatusRunning}, hang: true},
	}
	for name, gw := range cases {
		t.Run(name, func(t *testing.T) {
			e, pv := newPreviewEnv(t)
			e.svc.gw = gw
			e.svc.cfg.PreviewWait = 80 * time.Millisecond
			_, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
			if errReason(err) != reasonThirdParty {
				t.Fatalf("应是可重试的第三方错误：%v", err)
			}
			if e.ps.quota[pk(uid, e.svc.previewDay())] != 0 || len(e.ps.rows) != 0 {
				t.Fatalf("不占额度、不留占坑：%v rows=%d", e.ps.quota, len(e.ps.rows))
			}
		})
	}
}

type stubGW struct {
	call *aigateway.Call
	hang bool
}

func (g *stubGW) Submit(context.Context, aigateway.CallRequest, aigateway.SubmitOptions) (*aigateway.Call, error) {
	c := *g.call
	return &c, nil
}

func (g *stubGW) Get(ctx context.Context, _ string, _ time.Duration) (*aigateway.Call, error) {
	if g.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c := *g.call
	return &c, nil
}

func TestPreviewInProgressElsewhere(t *testing.T) {
	e, pv := newPreviewEnv(t)
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	// 别的调用正占着坑（刚占，没超时）：本次不出图、退额度、提示稍后
	e.ps.rows[pk(uid, p.PlanHash)] = &model.Preview{ID: 99, UserID: uid, PlanHash: p.PlanHash, Status: model.PreviewGenerating, UpdatedAt: e.clock}
	_, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if errReason(err) != reasonInProgress || e.ps.quota[pk(uid, e.svc.previewDay())] != 0 || len(e.gw.submits) != 0 {
		t.Fatalf("应提示正在生成且不占额度：%v quota=%v", err, e.ps.quota)
	}
	// 占坑超时（进程崩溃遗留）：可接手出图
	e.clock = e.clock.Add(previewClaimStale + time.Second)
	if v, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo); err != nil || v.ImageRid != previewImageRid {
		t.Fatalf("超时占坑应可接手：%v %+v", err, v)
	}
}

func TestPreviewGuards(t *testing.T) {
	t.Run("未装配或关闭回7003", func(t *testing.T) {
		e, pv := newPreviewEnv(t)
		e.svc.cfg.PreviewOff = true
		if _, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo); errCode(err) != xerr.NotReady {
			t.Fatalf("关闭应 7003：%v", err)
		}
		e.svc.cfg.PreviewOff, e.svc.previews = false, nil
		if _, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo); errCode(err) != xerr.NotReady {
			t.Fatalf("未装配应 7003：%v", err)
		}
	})
	t.Run("方案不存在或不是本人的", func(t *testing.T) {
		e, pv := newPreviewEnv(t)
		if _, err := e.svc.PreviewFrame(e.ctx, uid+1, pv.PlanNo); errCode(err) != xerr.NotFound {
			t.Fatalf("别人的方案应找不到：%v", err)
		}
	})
	t.Run("只改台词没有可预览的画面", func(t *testing.T) {
		e, _ := newPreviewEnv(t)
		a := e.seedAnalysis(12)
		pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5",
			Annotations: Annotations{Dims: map[string]DimEdit{"line": {Mode: ModeDescribe, Note: "改成促销口播"}}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
		if errCode(err) != xerr.ParamError || e.ps.quota[pk(uid, e.svc.previewDay())] != 0 {
			t.Fatalf("应拒绝且不占额度：%v", err)
		}
	})
	t.Run("没有原片帧", func(t *testing.T) {
		e, pv := newPreviewEnv(t)
		for _, x := range e.store.analyses {
			x.State = nil
		}
		_, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
		if err == nil || e.ps.quota[pk(uid, e.svc.previewDay())] != 0 {
			t.Fatalf("没有帧应报错且不占额度：%v", err)
		}
	})
	t.Run("被取代的方案", func(t *testing.T) {
		e, pv := newPreviewEnv(t)
		for _, p := range e.store.plans {
			p.Status = model.PlanStale
		}
		if _, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo); errReason(err) != ReasonPlanStale {
			t.Fatalf("应 PLAN_STALE：%v", err)
		}
	})
}

func errCode(err error) int {
	c, _, _ := xerr.Parse(err)
	return c
}

func TestNearestFrame(t *testing.T) {
	fs := []FrameOut{{T: 0, Rid: "a"}, {T: 4, Rid: "b"}, {T: 8, Rid: "c"}, {T: 9, Rid: ""}}
	if f, ok := nearestFrame(fs, 5); !ok || f.Rid != "b" {
		t.Fatalf("%+v", f)
	}
	if f, _ := nearestFrame(fs, 100); f.Rid != "c" {
		t.Fatalf("空 rid 的帧不参与：%+v", f)
	}
	if _, ok := nearestFrame(nil, 1); ok {
		t.Fatal("没有帧应 !ok")
	}
}

// GET 只读定妆图：没出过 → 4001；出过 → 现签新地址、回放标记、不占额度、不再出图；别人的方案看不到。
func TestGetPreviewReadOnly(t *testing.T) {
	e, pv := newPreviewEnv(t)
	if _, err := e.svc.GetPreview(e.ctx, uid, pv.PlanNo); errReason(err) != "RESOURCE_NOT_FOUND" {
		t.Fatalf("没出过图应 4001：%v", err)
	}
	if len(e.gw.submits) != 0 || e.ps.quota[pk(uid, e.svc.previewDay())] != 0 {
		t.Fatalf("只读不得出图 / 占额度：submits=%d", len(e.gw.submits))
	}
	first, err := e.svc.PreviewFrame(e.ctx, uid, pv.PlanNo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.GetPreview(e.ctx, uid, pv.PlanNo)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRid != first.ImageRid || got.QuotaUsed != 1 || !got.Replay || got.ImageURL == "" || got.FrameURL == "" || got.URLExpiresAt == nil {
		t.Fatalf("应返回已存图与新签地址：%+v", got)
	}
	if len(e.gw.submits) != 1 || e.ps.quota[pk(uid, e.svc.previewDay())] != 1 {
		t.Fatalf("读取不得再出图 / 占额度")
	}
	// 额度用完、开关关闭也照常可读
	e.ps.quota[pk(uid, e.svc.previewDay())] = 3
	e.svc.cfg.PreviewOff = true
	if _, err := e.svc.GetPreview(e.ctx, uid, pv.PlanNo); err != nil {
		t.Fatalf("额度用完 / 关闭后已有的图仍应可读：%v", err)
	}
	// 别人的方案：当作不存在
	if _, err := e.svc.GetPreview(e.ctx, uid+1, pv.PlanNo); errReason(err) != "RESOURCE_NOT_FOUND" {
		t.Fatalf("非属主应 4001：%v", err)
	}
}
