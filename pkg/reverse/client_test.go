package reverse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var img = DataURL("image/png", []byte("fake-png"))

type fake struct {
	t        *testing.T
	mu       sync.Mutex
	hits     map[string]int
	bodies   map[string][]map[string]any
	inflight atomic.Int32
	peak     atomic.Int32
	delay    atomic.Int64
	handler  func(path string, body map[string]any) (int, any)
}

func newFake(t *testing.T, h func(path string, body map[string]any) (int, any)) (*fake, *httptest.Server) {
	f := &fake{t: t, hits: map[string]int{}, bodies: map[string][]map[string]any{}, handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			p := f.peak.Load()
			if n <= p || f.peak.CompareAndSwap(p, n) {
				break
			}
		}
		if d := f.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("%s: bad json %v", r.URL.Path, err)
			}
		}
		f.mu.Lock()
		f.hits[r.URL.Path]++
		f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], body)
		f.mu.Unlock()
		code, out := f.handler(r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fake) hit(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[p]
}

func (f *fake) body(p string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies[p]...)
}

func newClient(url string) *Client {
	return New(Config{BaseURL: url, RetryBackoff: time.Millisecond})
}

func TestDefaults(t *testing.T) {
	c := New(Config{BaseURL: "http://x/"})
	if c.Timeout != 200*time.Second || c.Concurrency != 3 || c.MaxRetries != 1 || c.BaseURL != "http://x" {
		t.Fatalf("defaults: %+v", c)
	}
	if New(Config{}).Enabled() {
		t.Fatal("empty BaseURL must be disabled")
	}
	if _, err := New(Config{}).Status(context.Background()); !IsNotConfigured(err) {
		t.Fatalf("want not configured, got %v", err)
	}
	if got := DataURL("", []byte{1, 2}); got != "data:image/jpeg;base64,AQI=" {
		t.Fatalf("DataURL = %s", got)
	}
}

func TestStatus(t *testing.T) {
	_, srv := newFake(t, func(path string, _ map[string]any) (int, any) {
		return 200, map[string]any{"service": "prompt-reverse", "version": "2026-10-30.merged", "mock": false,
			"llm_configured": true, "model": "deepseek-flash", "model_label": "DeepSeek V4 Flash · 读图", "vision": true}
	})
	st, err := newClient(srv.URL).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Service != "prompt-reverse" || !st.LLMConfigured || !st.Vision || st.Model != "deepseek-flash" || st.ModelLabel == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestPoseSourceAndPrompt(t *testing.T) {
	f, srv := newFake(t, func(path string, body map[string]any) (int, any) {
		switch path {
		case PathPoseSource:
			return 200, map[string]any{
				"source": map[string]any{"has_person": true, "gender_age": "成年女性", "face": "鹅蛋脸",
					"outfit": []string{"白色衬衫"}, "accessories": []string{}, "framing": "半身", "extra_new": "x"},
				"meta": map[string]any{"model": "deepseek-flash", "elapsed_ms": 1200, "usage": map[string]any{"total_tokens": 10}},
			}
		case PathPosePrompt:
			return 200, map[string]any{
				"prompt": "保持图1人物……",
				"pose":   map[string]any{"has_person": true, "orientation_deg": "前侧45°", "arms": "双手叉腰", "confidence": "高", "face_visible": "左脸"},
				"meta":   map[string]any{"model": "deepseek-flash", "elapsed_ms": 900},
			}
		}
		return 404, map[string]any{"error": "not found"}
	})
	c := newClient(srv.URL)
	src, err := c.PoseSource(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Source.HasPerson || src.Source.GenderAge != "成年女性" || src.Source.Outfit[0] != "白色衬衫" || src.Source.Framing != "半身" || src.Meta.ElapsedMs != 1200 {
		t.Fatalf("source = %+v", src)
	}
	if f.body(PathPoseSource)[0]["image"] != img {
		t.Fatal("pose/source must send image")
	}
	pp, err := c.PosePrompt(context.Background(), src, img, "  抬头  ")
	if err != nil {
		t.Fatal(err)
	}
	if pp.Prompt == "" || pp.Pose.OrientationDeg != "前侧45°" || pp.Pose.Confidence != "高" || pp.Pose.FaceVisible != "左脸" {
		t.Fatalf("prompt = %+v", pp)
	}
	b := f.body(PathPosePrompt)[0]
	srcSent, _ := b["source"].(map[string]any)
	if b["image"] != img || b["notes"] != "抬头" || srcSent["extra_new"] != "x" || srcSent["face"] != "鹅蛋脸" {
		t.Fatalf("pose/prompt body = %v (source must be passed back verbatim)", b)
	}
	if _, ok := b["mode"]; ok {
		t.Fatal("mode must be omitted for long version")
	}
}

func TestPoseSource422NotRetried(t *testing.T) {
	f, srv := newFake(t, func(string, map[string]any) (int, any) {
		return 422, map[string]any{"error": "原图中没有识别到人物，请换一张有模特的照片"}
	})
	c := New(Config{BaseURL: srv.URL, MaxRetries: 3, RetryBackoff: time.Millisecond})
	_, err := c.PoseSource(context.Background(), img)
	if !IsRejected(err) || IsRetryable(err) {
		t.Fatalf("want rejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "没有识别到人物") {
		t.Fatalf("message lost: %v", err)
	}
	if f.hit(PathPoseSource) != 1 {
		t.Fatalf("422 retried: hits=%d", f.hit(PathPoseSource))
	}
}

func TestErrorKinds(t *testing.T) {
	var n atomic.Int32
	f, srv := newFake(t, func(path string, _ map[string]any) (int, any) {
		switch path {
		case PathCropCheck:
			return 502, map[string]any{"error": "裁切质检失败：未配置 AI 网关（AIG_BASE_URL / AIG_SECRET）"}
		case PathPoseSource:
			if n.Add(1) == 1 {
				return 502, map[string]any{"error": "读图返回 503，请稍后重试"}
			}
			return 200, map[string]any{"source": map[string]any{"has_person": true}, "meta": map[string]any{}}
		}
		return 500, "boom"
	})
	c := New(Config{BaseURL: srv.URL, MaxRetries: 2, RetryBackoff: time.Millisecond})
	_, err := c.CropCheck(context.Background(), []CropCheckItem{{Data: img, Label: "上衣·领口"}})
	if !IsNotConfigured(err) || IsRetryable(err) {
		t.Fatalf("want not configured, got %v", err)
	}
	if f.hit(PathCropCheck) != 1 {
		t.Fatalf("not-configured retried: %d", f.hit(PathCropCheck))
	}
	// 5xx 重试后成功
	if _, err := c.PoseSource(context.Background(), img); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if f.hit(PathPoseSource) != 2 {
		t.Fatalf("hits = %d", f.hit(PathPoseSource))
	}
	// 网络错误可重试
	dead := New(Config{BaseURL: "http://127.0.0.1:1", MaxRetries: -1})
	if _, err := dead.Status(context.Background()); !IsRetryable(err) {
		t.Fatalf("transport should be retryable: %v", err)
	}
}

func TestPrecheck(t *testing.T) {
	_, srv := newFake(t, func(string, map[string]any) (int, any) { return 200, map[string]any{} })
	c := newClient(srv.URL)
	if _, err := c.PoseSource(context.Background(), "https://x/a.jpg"); !IsRejected(err) {
		t.Fatalf("non data url: %v", err)
	}
	big := "data:image/jpeg;base64," + strings.Repeat("A", MaxImageBytes)
	if _, err := c.PoseSource(context.Background(), big); !IsRejected(err) {
		t.Fatalf("12MB: %v", err)
	}
	// 每张 <12MB 但合计 >24MB
	half := "data:image/jpeg;base64," + strings.Repeat("A", 9<<20)
	_, err := c.Analyze(context.Background(), AnalyzeReq{Category: "top", Images: []AnalyzeImage{{Role: "上衣", Data: half}, {Role: "上衣", Data: half}, {Role: RoleModel, Data: half}}})
	if !IsRejected(err) || !strings.Contains(err.Error(), "请求体") {
		t.Fatalf("24MB: %v", err)
	}
}

func TestAnalyze(t *testing.T) {
	f, srv := newFake(t, func(string, map[string]any) (int, any) {
		return 200, map[string]any{"prompt": "P", "warning": "读图返回 429，已改用文字信息生成",
			"meta": map[string]any{"category": "compose", "model": "deepseek-flash", "degraded": true, "vision": false, "images": 0, "elapsed_ms": 50}}
	})
	req := AnalyzeReq{Category: "compose", Items: []string{"top", "bottom"}, SetMode: "two", Fabric: "上衣：真丝；下装：牛仔",
		Notes: "n", Lengths: map[string]string{"bottom": "九分"}, Sleeves: map[string]string{"top": "长袖"}, Deep: true,
		Images: []AnalyzeImage{{Role: "上衣", Data: img}, {Role: RoleDetail, Label: "上衣·领口", Data: img}, {Role: RoleModel, Data: img}}}
	r, err := newClient(srv.URL).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Meta.Degraded || r.Warning == "" || r.Prompt != "P" || r.Meta.ElapsedMs != 50 {
		t.Fatalf("resp = %+v", r)
	}
	b := f.body(PathAnalyze)[0]
	if b["category"] != "compose" || b["set_mode"] != "two" || b["fabric"] != "上衣：真丝；下装：牛仔" || b["deep"] != true {
		t.Fatalf("body = %v", b)
	}
	if b["lengths"].(map[string]any)["bottom"] != "九分" || b["sleeves"].(map[string]any)["top"] != "长袖" {
		t.Fatalf("lengths/sleeves = %v", b)
	}
	ims := b["images"].([]any)
	if ims[1].(map[string]any)["role"] != RoleDetail || ims[1].(map[string]any)["label"] != "上衣·领口" {
		t.Fatalf("images = %v", ims)
	}
}

func TestAnalyzeBatch(t *testing.T) {
	f, srv := newFake(t, func(_ string, body map[string]any) (int, any) {
		models := body["models"].([]any)
		var res []map[string]any
		for i, m := range models {
			r := map[string]any{"index": i, "model_id": m.(map[string]any)["id"], "prompt": "P", "meta": map[string]any{"degraded": false, "vision": true}, "warning": ""}
			if i == 1 {
				r = map[string]any{"index": i, "model_id": m.(map[string]any)["id"], "prompt": "", "meta": map[string]any{}, "warning": "", "error": "boom"}
			}
			res = append(res, r)
		}
		return 200, map[string]any{"results": res, "meta": map[string]any{"models": len(models), "concurrency": body["concurrency"], "prompt_version": "old", "target_model": "", "elapsed_ms": 7}}
	})
	c := newClient(srv.URL)
	r, err := c.AnalyzeBatch(context.Background(), AnalyzeBatchReq{
		AnalyzeReq:  AnalyzeReq{Category: "top", Items: []string{"top"}, Images: []AnalyzeImage{{Role: "上衣", Data: img}}},
		Models:      []BatchModel{{ID: "m-1", Data: img}, {ID: "m-2", Data: img}},
		Concurrency: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Results) != 2 || r.Results[1].Error != "boom" || r.Results[0].ModelID != "m-1" || r.Meta.Concurrency != 6 || r.Meta.Models != 2 {
		t.Fatalf("resp = %+v", r)
	}
	b := f.body(PathAnalyzeBatch)[0]
	if b["concurrency"].(float64) != 6 || b["category"] != "top" || len(b["models"].([]any)) != 2 {
		t.Fatalf("body = %v (AnalyzeReq must be inlined)", b)
	}
	ten := make([]BatchModel, 10)
	for i := range ten {
		ten[i] = BatchModel{ID: "x", Data: img}
	}
	if _, err := c.AnalyzeBatch(context.Background(), AnalyzeBatchReq{Models: ten}); !IsRejected(err) {
		t.Fatalf("10 models: %v", err)
	}
}

func TestCropPlanSplitsAndRemaps(t *testing.T) {
	f, srv := newFake(t, func(_ string, body map[string]any) (int, any) {
		ims := body["images"].([]any)
		if len(ims) > CropPlanBatch {
			return 500, map[string]any{"error": "too many"}
		}
		var plans []map[string]any
		for i, im := range ims {
			angle := im.(map[string]any)["angle"]
			plans = append(plans, map[string]any{"index": i, "angle": angle, "garment_box": []float64{0, 0, 1, 1},
				"crops": []map[string]any{{"label": im.(map[string]any)["role"].(string) + "·领口", "box": []float64{0.1, 0.1, 0.2, 0.2}, "confidence": 0.86}}})
		}
		meta := map[string]any{"images": len(ims), "plans": len(plans), "elapsed_ms": 10 * len(ims)}
		if len(ims) == 5 {
			meta["mode"] = "per-item"
		}
		return 200, map[string]any{"plans": plans, "meta": meta}
	})
	var ims []CropPlanImage
	angles := []string{"front", "back", "side", "detail", "flat", "front", "back", "side", "detail"}
	for i := 0; i < 9; i++ {
		ims = append(ims, CropPlanImage{Data: img, Angle: angles[i], Role: []string{"上衣", "下装", "鞋子"}[i/3]})
	}
	r, err := newClient(srv.URL).CropPlan(context.Background(), CropPlanReq{Category: "compose", Items: []string{"top", "bottom", "shoes"}, Images: ims})
	if err != nil {
		t.Fatal(err)
	}
	if f.hit(PathCropPlan) != 2 {
		t.Fatalf("batches = %d", f.hit(PathCropPlan))
	}
	sizes := map[int]bool{}
	for _, b := range f.body(PathCropPlan) {
		sizes[len(b["images"].([]any))] = true
		if b["category"] != "compose" || len(b["items"].([]any)) != 3 {
			t.Fatalf("batch body lost fields: %v", b)
		}
	}
	if !sizes[5] || !sizes[4] {
		t.Fatalf("want 5+4 split, got %v", sizes)
	}
	if len(r.Plans) != 9 {
		t.Fatalf("plans = %d", len(r.Plans))
	}
	seen := map[int]bool{}
	for _, p := range r.Plans {
		seen[p.Index] = true
		if p.Angle != angles[p.Index] {
			t.Fatalf("plan %d angle %s, want %s (index not remapped)", p.Index, p.Angle, angles[p.Index])
		}
		wantPrefix := []string{"上衣", "下装", "鞋子"}[p.Index/3]
		if !strings.HasPrefix(p.Crops[0].Label, wantPrefix) || len(p.Crops[0].Box) != 4 {
			t.Fatalf("plan %d crops %+v", p.Index, p.Crops)
		}
	}
	if len(seen) != 9 {
		t.Fatalf("indexes = %v", seen)
	}
	if r.Meta.Images != 9 || r.Meta.Plans != 9 || r.Meta.Mode != "per-item,candidates" && r.Meta.Mode != "candidates,per-item" || len(r.Batches) != 2 {
		t.Fatalf("meta = %+v", r.Meta)
	}
}

func TestSplitEven(t *testing.T) {
	cases := map[int][]int{1: {1}, 6: {6}, 7: {4, 3}, 9: {5, 4}, 12: {6, 6}, 13: {5, 4, 4}}
	for n, want := range cases {
		got := splitEven(n, 6)
		if len(got) != len(want) {
			t.Fatalf("n=%d got %v", n, got)
		}
		for i, g := range got {
			if g[1]-g[0] != want[i] {
				t.Fatalf("n=%d got %v", n, got)
			}
		}
	}
}

func TestCropCheckSplitsAndRemaps(t *testing.T) {
	f, srv := newFake(t, func(_ string, body map[string]any) (int, any) {
		items := body["items"].([]any)
		if len(items) > CropCheckBatch {
			return 500, map[string]any{"error": "too many"}
		}
		var res []map[string]any
		for i, it := range items {
			ok := !strings.HasSuffix(it.(map[string]any)["label"].(string), "-bad")
			res = append(res, map[string]any{"index": i, "ok": ok, "confidence": 0.9})
		}
		return 200, map[string]any{"results": res, "meta": map[string]any{"items": len(items), "elapsed_ms": 3}}
	})
	var items []CropCheckItem
	for i := 0; i < 13; i++ {
		label := "上衣·领口"
		if i == 12 || i == 3 {
			label += "-bad"
		}
		items = append(items, CropCheckItem{Data: img, Label: label})
	}
	res, err := newClient(srv.URL).CropCheck(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	if f.hit(PathCropCheck) != 2 || len(res) != 13 {
		t.Fatalf("hits=%d res=%d", f.hit(PathCropCheck), len(res))
	}
	for _, r := range res {
		wantOK := r.Index != 12 && r.Index != 3
		if r.OK != wantOK {
			t.Fatalf("index %d ok=%v (remap broken)", r.Index, r.OK)
		}
	}
}

func TestCropCheckPartialFailure(t *testing.T) {
	_, srv := newFake(t, func(_ string, body map[string]any) (int, any) {
		items := body["items"].([]any)
		if len(items) == 1 {
			return 502, map[string]any{"error": "读图返回 500，请稍后重试"}
		}
		return 200, map[string]any{"results": []map[string]any{{"index": 0, "ok": false, "confidence": 0.95}}, "meta": map[string]any{"items": len(items)}}
	})
	items := make([]CropCheckItem, 13)
	for i := range items {
		items[i] = CropCheckItem{Data: img, Label: "x"}
	}
	res, err := New(Config{BaseURL: srv.URL, MaxRetries: -1}).CropCheck(context.Background(), items)
	if err == nil || len(res) != 1 || res[0].Index != 0 {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	f, srv := newFake(t, func(_ string, body map[string]any) (int, any) {
		return 200, map[string]any{"prompt": "P", "meta": map[string]any{}, "warning": ""}
	})
	f.delay.Store(int64(30 * time.Millisecond))
	c := New(Config{BaseURL: srv.URL, Concurrency: 2})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Analyze(context.Background(), AnalyzeReq{Category: "top", Images: []AnalyzeImage{{Role: "上衣", Data: img}}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := f.peak.Load(); p > 2 || p < 1 {
		t.Fatalf("peak in-flight = %d, want ≤2", p)
	}
	if f.hit(PathAnalyze) != 8 {
		t.Fatalf("hits = %d", f.hit(PathAnalyze))
	}
}

func TestCancelledContextNotRetried(t *testing.T) {
	f, srv := newFake(t, func(string, map[string]any) (int, any) { return 200, map[string]any{} })
	f.delay.Store(int64(200 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c := New(Config{BaseURL: srv.URL, MaxRetries: 3, RetryBackoff: time.Millisecond})
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("want error")
	}
	time.Sleep(250 * time.Millisecond)
	if f.hit(PathStatus) > 1 {
		t.Fatalf("retried after ctx done: %d", f.hit(PathStatus))
	}
}
