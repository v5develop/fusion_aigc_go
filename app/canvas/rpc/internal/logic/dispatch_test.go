package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"

	"google.golang.org/protobuf/proto"
)

func testConfig() config.Config {
	var c config.Config
	c.EntryCodes.Image, c.EntryCodes.Video, c.EntryCodes.Crop, c.EntryCodes.I2P = "canvas_image", "canvas_video", "canvas_tool_crop", "canvas_tool_i2p"
	c.I2P.ModelGroup, c.I2P.MaxTokens = "vlm.qwen-3.8-flash", 800
	c.FrameModelGroup = "vid.seedance-2"
	c.MaxCostCNY.Image, c.MaxCostCNY.Video, c.MaxCostCNY.I2P = "5.00", "60.00", "1.00"
	c.Timeouts.Image, c.Timeouts.Video, c.Timeouts.Tool, c.Timeouts.I2P = 300, 900, 600, 180
	c.Limits.MaxPromptLen, c.Limits.MaxSnapshotBytes, c.Limits.MaxNodes, c.Limits.MaxTitleLen, c.Limits.MaxPromptsPerUser = 3000, 2<<20, 2000, 100, 500
	c.TaskIDPrefix = "cv"
	return c
}

func testCore() *core {
	return &core{s: &svc.ServiceContext{Config: testConfig(), Gateway: aigateway.New(aigateway.Config{}), Java: javatool.New("", 0)}, now: time.Now}
}

func TestBackendDispatchTable(t *testing.T) {
	want := map[string]model.Backend{
		"image": model.BackendGateway, "video": model.BackendGateway, "tool:i2p": model.BackendGateway,
		"tool:crop": model.BackendMedia, "tool:upscale": model.BackendJava, "tool:outpaint": model.BackendJava,
		"tool:mask": model.BackendJava, "tool:inpaint": model.BackendJava, "tool:tryon": model.BackendStudio,
		"tool_upscale:": model.BackendJava,
	}
	for in, b := range want {
		kind, tool, _ := strings.Cut(in, ":")
		k, err := ResolveKind(kind, tool)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if model.BackendOf(k) != b {
			t.Fatalf("%s → %s, want %s", in, model.BackendOf(k), b)
		}
	}
	if _, err := ResolveKind("tool", "paint"); err == nil {
		t.Fatal("未知工具应拒绝")
	}
	if _, err := ResolveKind("audio", ""); err == nil {
		t.Fatal("未知 kind 应拒绝")
	}
	if k, _ := ResolveKind("", "crop"); k != model.KindToolCrop {
		t.Fatal("kind 为空但带 tool 按工具")
	}
}

func idx(i int32) *int32 { return &i }

func TestBuildSpecImage(t *testing.T) {
	c := testCore()
	req := &canvas.RunNodeRequest{Kind: "image", Model: "Banana", ModelId: "banana-v1", Prompt: "白底", Ratio: "1:1", Size: "2K", Count: 2, Quality: "中",
		References: []*canvas.RunReference{
			{Handle: "image", Index: 2, NodeId: "b", Image: "https://x/b.png", AssetId: "88"},
			{Handle: "prompt", NodeId: "t", Text: "棚拍"},
			{Handle: "image", Index: 1, NodeId: "a", Image: "https://x/stale.png", CandidateIndex: idx(1)},
		}}
	sp, err := c.buildSpec(req, func(node string, i int) string {
		if node == "a" && i == 1 {
			return "https://lib/a-1.png"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if sp.Backend != model.BackendGateway || sp.ModelGroup != "img.nano-banana-2.1" || sp.Count != 2 || sp.Size != "2K" {
		t.Fatalf("spec=%+v", sp)
	}
	if sp.Prompt != "棚拍\n白底" {
		t.Fatalf("提示词口文本在前: %q", sp.Prompt)
	}
	if len(sp.Images) != 2 || sp.Images[0].URL != "https://lib/a-1.png" || sp.Images[1].URL != "https://x/b.png" {
		t.Fatalf("按 index 排序、候选取服务端记录: %+v", sp.Images)
	}
	if sp.Quality != "" {
		t.Fatal("Banana 不下发 quality")
	}
	if len(sp.AssetIDs) != 1 || sp.AssetIDs[0] != "88" {
		t.Fatal("asset_id 进 relatedIds")
	}
	bad := []struct {
		mut  func(r *canvas.RunNodeRequest)
		want string
	}{
		{func(r *canvas.RunNodeRequest) { r.Model, r.ModelId = "MJ", "mj" }, "不支持的模型"},
		{func(r *canvas.RunNodeRequest) { r.Count = 5 }, "count 取值"},
		{func(r *canvas.RunNodeRequest) { r.Ratio = "7:3" }, "不支持比例"},
		{func(r *canvas.RunNodeRequest) { r.Size = "8K" }, "不支持分辨率"},
		{func(r *canvas.RunNodeRequest) { r.Prompt, r.References = "", nil }, "提示词为空"},
		{func(r *canvas.RunNodeRequest) { r.Prompt = strings.Repeat("字", 3001) }, "提示词最多"},
		{func(r *canvas.RunNodeRequest) {
			r.References = []*canvas.RunReference{{Handle: "image", Image: "data:image/png;base64,AA"}}
		}, "http(s)"},
	}
	for _, b := range bad {
		r := proto.Clone(req).(*canvas.RunNodeRequest)
		b.mut(r)
		if _, err := c.buildSpec(r, nil); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Fatalf("want %q, got %v", b.want, err)
		}
	}
}

func TestBuildSpecVideoFrames(t *testing.T) {
	c := testCore()
	req := &canvas.RunNodeRequest{Kind: "video", Model: "Seedance 2.5", Prompt: "走秀", Ratio: "16:9", Size: "720p", Duration: "6s", Sound: false,
		References: []*canvas.RunReference{{Handle: "image1", Image: "https://x/f.png"}, {Handle: "image2", Image: "https://x/l.png"}}}
	sp, err := c.buildSpec(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp.ModelGroup != "vid.seedance-2" || sp.First == "" || sp.Last == "" || sp.DurationS != 6 || sp.Count != 1 {
		t.Fatalf("2.5 不支持首尾帧，应换 seedance-2: %+v", sp)
	}
	req.References = append(req.References, &canvas.RunReference{Handle: "image", Image: "https://x/r.png"})
	if _, err := c.buildSpec(req, nil); err == nil || !strings.Contains(err.Error(), "首尾帧") {
		t.Fatalf("首尾帧不能与参考图同用: %v", err)
	}
	req.References = nil
	req.Duration = "7s"
	if _, err := c.buildSpec(req, nil); err == nil {
		t.Fatal("时长不在可选项应拒绝")
	}
}

func TestGatewayRequestsShape(t *testing.T) {
	c := testCore()
	run := &model.NodeRun{ID: 9, Kind: model.KindVideo, CanvasID: "cv_x", UserID: 7, TaskID: sql.NullString{String: "cv_t", Valid: true}}
	sp := &runSpec{Kind: model.KindVideo, ModelGroup: "vid.seedance-2", Prompt: "p", DurationS: 4, Size: "720p", Ratio: "16:9", First: "https://x/f.png", Sound: false}
	reqs, err := c.gatewayRequests(context.Background(), run, sp)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(reqs[0])
	s := string(b)
	for _, want := range []string{`"operation":"video.generate"`, `"duration_s":4`, `"resolution":"720p"`, `"generate_audio":false`,
		`"first_frame":{"uri":"https://x/f.png"}`, `"max_cost":"60.00"`, `"deadline_at":`} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺 %s: %s", want, s)
		}
	}
	if strings.Contains(s, "reference_images") || strings.Contains(s, "last_frame") {
		t.Fatalf("空字段必须省略（extra=forbid）: %s", s)
	}
	// 图片 count=3 → 3 个调用，Seedream 1K 升 2K，GPT 缺省 quality=medium
	run.Kind = model.KindImage
	m, _ := c.s.Config.FindModel("SD5P", "", "image")
	reqs, _ = c.gatewayRequests(context.Background(), run, &runSpec{Kind: model.KindImage, Model: m, ModelGroup: m.ModelGroup, Prompt: "p", Count: 3, Size: "1K"})
	b, _ = json.Marshal(reqs[0].Input)
	if len(reqs) != 3 || !strings.Contains(string(b), `"resolution":"2K"`) || !strings.Contains(string(b), `"aspect_ratio":"1:1"`) || strings.Contains(string(b), "quality") {
		t.Fatalf("seedream: %d %s", len(reqs), b)
	}
	g, _ := c.s.Config.FindModel("GPT", "", "image")
	reqs, _ = c.gatewayRequests(context.Background(), run, &runSpec{Kind: model.KindImage, Model: g, ModelGroup: g.ModelGroup, Prompt: "p", Count: 1, Size: "1K",
		Images: []refMedia{{URL: "https://x/a.png"}}})
	b, _ = json.Marshal(reqs[0])
	if !strings.Contains(string(b), `"operation":"image.edit"`) || !strings.Contains(string(b), `"quality":"medium"`) || !strings.Contains(string(b), `"images":[{"uri":"https://x/a.png"}]`) {
		t.Fatalf("gpt edit: %s", b)
	}
	if k, err := gatewayIdem(run, 2); err != nil || k != "canvas:cv_t:2" {
		t.Fatal("幂等键")
	}
	// i2p：vlm 组、图片块字段 uri
	run.Kind = model.KindToolI2P
	reqs, _ = c.gatewayRequests(context.Background(), run, &runSpec{Kind: model.KindToolI2P, ModelGroup: "vlm.qwen-3.8-flash", Images: []refMedia{{URL: "https://x/a.png"}}})
	b, _ = json.Marshal(reqs[0])
	if !strings.Contains(string(b), `"operation":"chat.completion"`) || !strings.Contains(string(b), `{"type":"image_url","uri":"https://x/a.png"}`) || !strings.Contains(string(b), `"role":"system"`) {
		t.Fatalf("i2p: %s", b)
	}
}

func call(status string, mod func(c *aigateway.Call)) *aigateway.Call {
	c := &aigateway.Call{ID: "call_" + status, Status: status}
	if mod != nil {
		mod(c)
	}
	return c
}

func imgResult(rid string) *aigateway.Result {
	return &aigateway.Result{Outputs: []aigateway.OutputItem{{Status: aigateway.StatusSucceeded, Media: &aigateway.MediaOut{Rid: rid, URL: "https://g/" + rid, Width: 64, Height: 64}}}}
}

func TestClassifyGateway(t *testing.T) {
	ok := call(aigateway.StatusSucceeded, func(c *aigateway.Call) {
		c.Result = imgResult("r1")
		c.Cost = &aigateway.Cost{AmountCNY: "0.50"}
	})
	if o := ClassifyGateway(model.KindImage, []*aigateway.Call{ok, call(aigateway.StatusRunning, nil)}); o.done {
		t.Fatal("有未终态调用不算完成")
	}
	if o := ClassifyGateway(model.KindImage, []*aigateway.Call{ok, nil}); o.done {
		t.Fatal("缺调用记录不算完成")
	}
	failed := call(aigateway.StatusFailed, func(c *aigateway.Call) { c.Failure = &aigateway.Failure{Class: "CONTENT_POLICY"} })
	o := ClassifyGateway(model.KindImage, []*aigateway.Call{ok, failed})
	if !o.done || !o.ok || o.actual != 1 || len(o.outputs) != 1 || math.Abs(o.costCNY-0.5) > 1e-9 {
		t.Fatalf("部分成功按成功张数: %+v", o)
	}
	o = ClassifyGateway(model.KindImage, []*aigateway.Call{failed})
	if !o.done || o.ok || o.code != "CONTENT_BLOCKED" || o.review {
		t.Fatalf("内容拦截: %+v", o)
	}
	o = ClassifyGateway(model.KindImage, []*aigateway.Call{call(aigateway.StatusUnknown, nil), failed})
	if o.ok || !o.review || o.code != "UPSTREAM_UNKNOWN" {
		t.Fatalf("UNKNOWN 待对账不退款: %+v", o)
	}
	o = ClassifyGateway(model.KindImage, []*aigateway.Call{call(aigateway.StatusCancelled, nil)})
	if o.ok || o.code != "CANCELED" {
		t.Fatalf("取消: %+v", o)
	}
	o = ClassifyGateway(model.KindImage, []*aigateway.Call{call(aigateway.StatusSucceeded, func(c *aigateway.Call) { c.Result = &aigateway.Result{} })})
	if o.ok || o.code != "EMPTY_RESULT" {
		t.Fatalf("成功但无产物: %+v", o)
	}
	v := call(aigateway.StatusSucceeded, func(c *aigateway.Call) {
		c.Result = &aigateway.Result{Video: &aigateway.MediaOut{Rid: "v"}, Cover: &aigateway.MediaOut{Rid: "c"}}
	})
	if o := ClassifyGateway(model.KindVideo, []*aigateway.Call{v}); !o.ok || o.video.RID != "v" || o.cover.RID != "c" {
		t.Fatalf("视频: %+v", o)
	}
	var res aigateway.Result
	_ = json.Unmarshal([]byte(`{"operation":"chat.completion","message":{"role":"assistant","content":"  一只橘猫，柔光  "},"finish_reason":"stop"}`), &res)
	tx := call(aigateway.StatusSucceeded, func(c *aigateway.Call) { c.Result = &res })
	if o := ClassifyGateway(model.KindToolI2P, []*aigateway.Call{tx}); !o.ok || o.text != "一只橘猫，柔光" {
		t.Fatalf("i2p: %+v", o)
	}
}

func TestClassifyJavaAndStudio(t *testing.T) {
	if ClassifyJava(&javatool.Task{Status: javatool.StatusRunning}).done || ClassifyJava(&javatool.Task{Status: javatool.StatusWaiting}).done {
		t.Fatal("运行中 / 待人工不是终态")
	}
	if o := ClassifyJava(&javatool.Task{Status: javatool.StatusSucceed, ResultURL: "https://j/1.png"}); !o.ok || o.outputs[0].URL != "https://j/1.png" {
		t.Fatalf("%+v", o)
	}
	if o := ClassifyJava(&javatool.Task{Status: javatool.StatusSucceed}); o.ok || o.code != "EMPTY_RESULT" {
		t.Fatal("成功但无地址")
	}
	if o := ClassifyJava(&javatool.Task{Status: javatool.StatusFailed, ErrorMessage: "点数不足"}); o.ok || o.msg != "点数不足" {
		t.Fatal("失败带原因")
	}
	if o := ClassifyJava(&javatool.Task{Status: javatool.StatusTimeout}); o.code != "TIMEOUT" {
		t.Fatal("超时")
	}
	if ClassifyStudio(&studio.BatchDetail{Batch: &studio.BatchInfo{Status: 3}}).done {
		t.Fatal("生图中不是终态")
	}
	b := &studio.BatchDetail{Batch: &studio.BatchInfo{Status: 6}, Tasks: []*studio.TaskInfo{
		{Status: 2, ResultUrl: "https://s/1.png", ResultWidth: 10, ResultHeight: 20}, {Status: 3}, {Status: 2, ResultUrl: "https://s/3.png"}}}
	if o := ClassifyStudio(b); !o.ok || o.actual != 2 || !o.outputs[0].Stable || o.outputs[0].W != 10 {
		t.Fatalf("部分成功: %+v", o)
	}
	if o := ClassifyStudio(&studio.BatchDetail{Batch: &studio.BatchInfo{Status: 5, ErrorReason: "切图失败"}}); o.ok || o.msg != "切图失败" {
		t.Fatal("失败")
	}
	if o := ClassifyStudio(&studio.BatchDetail{Batch: &studio.BatchInfo{Status: 7}}); o.code != "CANCELED" {
		t.Fatal("取消")
	}
}

func TestToolParams(t *testing.T) {
	b, err := CropBox(map[string]any{"shape": "circle", "x": json.Number("0.1"), "y": 0.2, "w": 0.5, "h": 0.9})
	if err != nil || b.X != 0.1 || math.Abs(b.H-0.8) > 1e-9 {
		t.Fatalf("圆形取外接矩形并裁进图内: %+v %v", b, err)
	}
	b, err = CropBox(map[string]any{"shape": "brush", "brush": []any{map[string]any{"x": 0.2, "y": 0.2, "r": 0.1}, map[string]any{"x": 0.5, "y": 0.6, "r": 0.1}}})
	if err != nil || math.Abs(b.X-0.1) > 1e-9 || math.Abs(b.W-0.5) > 1e-9 || math.Abs(b.H-0.6) > 1e-9 {
		t.Fatalf("笔刷外接矩形: %+v %v", b, err)
	}
	if _, err := CropBox(map[string]any{"x": 0.1}); err == nil {
		t.Fatal("缺 w/h 应拒绝")
	}
	pts := BoxPoints(NormBox{0.1, 0.2, 0.3, 0.4})
	if len(pts) != 4 || pts[1].X != 0.4 || pts[2].Y != 0.6 || pts[3].X != 0.1 {
		t.Fatalf("bbox 四点顺序左上右上右下左下: %+v", pts)
	}
	for in, want := range map[float64]string{2: "2k", 4: "4k"} {
		if r, _ := upscaleResolution(map[string]any{"scale": in}, ""); r != want {
			t.Fatal("scale→分辨率")
		}
	}
	if _, err := upscaleResolution(map[string]any{"scale": 3.0}, ""); err == nil {
		t.Fatal("3 倍不支持")
	}
	sp := &runSpec{Params: map[string]any{"garment_image": "https://x/g.png", "models": []any{map[string]any{"id": "m1", "image": "https://x/m1.png"}}}}
	if g, ms, err := tryonInputs(sp); err != nil || g != "https://x/g.png" || len(ms) != 1 {
		t.Fatalf("试穿 params: %v %v %v", g, ms, err)
	}
	sp = &runSpec{Params: map[string]any{}, Images: []refMedia{{URL: "https://x/m.png", Handle: "models"}, {URL: "https://x/g.png", Handle: "garment"}}}
	if g, ms, _ := tryonInputs(sp); g != "https://x/g.png" || ms[0] != "https://x/m.png" {
		t.Fatal("试穿按口取")
	}
	if _, _, err := tryonInputs(&runSpec{Params: map[string]any{}}); err == nil {
		t.Fatal("试穿缺图应拒绝")
	}
}

func TestMaskBounds(t *testing.T) {
	// 透明底 + 蓝色笔迹
	img := image.NewNRGBA(image.Rect(0, 0, 100, 50))
	for y := 10; y < 20; y++ {
		for x := 30; x < 60; x++ {
			img.Set(x, y, color.NRGBA{0, 0, 255, 128})
		}
	}
	b, ok := MaskBounds(img)
	if !ok || math.Abs(b.X-0.3) > 1e-9 || math.Abs(b.Y-0.2) > 1e-9 || math.Abs(b.W-0.3) > 1e-9 || math.Abs(b.H-0.2) > 1e-9 {
		t.Fatalf("透明底: %+v", b)
	}
	// 黑底白笔
	img2 := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for i := range img2.Pix {
		img2.Pix[i] = 0
		if i%4 == 3 {
			img2.Pix[i] = 255
		}
	}
	img2.Set(2, 3, color.White)
	img2.Set(5, 7, color.White)
	if b, ok := MaskBounds(img2); !ok || b.X != 0.2 || b.Y != 0.3 || math.Abs(b.W-0.4) > 1e-9 || math.Abs(b.H-0.5) > 1e-9 {
		t.Fatalf("黑底白笔: %+v", b)
	}
	if _, ok := MaskBounds(image.NewNRGBA(image.Rect(0, 0, 4, 4))); ok {
		t.Fatal("空蒙版")
	}
}

func TestJavaErrMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
		temp bool
	}{
		{&javatool.Error{HTTPStatus: 200, Code: 3003, Msg: "点数余额不足"}, xerr.InsufficientPoints, false},
		{&javatool.Error{HTTPStatus: 401, Code: 2001}, xerr.Unauthorized, false},
		{&javatool.Error{HTTPStatus: 200, Code: 1003, Msg: "图片不能为空"}, 1003, false},
		{&javatool.Error{HTTPStatus: 502}, 0, true},
		{&javatool.Error{HTTPStatus: 0, Msg: "dial"}, 0, true},
		{javatool.ErrNoBearer, xerr.NotReady, false},
	}
	for _, c := range cases {
		err := javaErr(c.err)
		if isTemporary(err) != c.temp {
			t.Fatalf("%v temporary=%v", c.err, isTemporary(err))
		}
		if !c.temp {
			if code, _, _ := xerr.Parse(err); code != c.code {
				t.Fatalf("%v → %d want %d", c.err, code, c.code)
			}
		}
	}
}

func TestProgressAndResult(t *testing.T) {
	c := testCore()
	now := time.Now()
	c.now = func() time.Time { return now }
	r := &model.NodeRun{ID: 3, Kind: model.KindImage, Status: model.RunRunning, CreatedAt: now.Add(-30 * time.Second),
		StartedAt: sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true}, Request: json.RawMessage(`{"model":"Banana"}`)}
	if p := c.progressOf(r); p < 40 || p > 60 {
		t.Fatalf("30s / 60s 预计 ≈ 50: %d", p)
	}
	r.StartedAt.Time = now.Add(-time.Hour)
	if c.progressOf(r) != 95 {
		t.Fatal("运行中最多 95")
	}
	r.Status, r.ErrorCode, r.ErrorReason, r.BillingStatus, r.Points = model.RunFailed, sql.NullString{String: "CONTENT_BLOCKED", Valid: true}, sql.NullString{String: "x", Valid: true}, model.BillRefunded, 40
	res := c.toResult(r)
	if res.Status != "failed" || res.Retryable || res.Credits != 0 || res.Model != "Banana" || res.RequestId != "3" {
		t.Fatalf("失败结果: %+v", res)
	}
	r.ErrorCode.String = "TIMEOUT"
	if !c.toResult(r).Retryable {
		t.Fatal("超时可重试")
	}
}
