package media

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/image/font/gofont/goregular"
)

// 视频做同款的媒体算子测试：素材全部用 ffmpeg 现生成（testsrc / sine），产物都在 t.TempDir() 下，用完即清。

func rawJSON(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	var m json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ingestRID 把 f.dir 下的文件摄入为 rid。
func ingestRID(t *testing.T, f *apiFixture, name, purpose string) string {
	t.Helper()
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest",
		map[string]any{"source": f.url(name), "purpose": purpose}, testToken, "k-ing-"+name+"-"+purpose))
	return res.Object.RID
}

// makeSpecClip 指定规格的测试视频。audioRate=0 不带音轨。gop 取大值让大部分时间点不在关键帧上。
func makeSpecClip(t *testing.T, path string, secs float64, size string, fps int, audioRate int) {
	t.Helper()
	args := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i",
		fmt.Sprintf("testsrc=duration=%v:size=%s:rate=%d", secs, size, fps)}
	if audioRate > 0 {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%v:sample_rate=%d", secs, audioRate))
	}
	args = append(args, "-pix_fmt", "yuv420p", "-g", "100", "-c:v", "libx264", "-shortest", path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v: %s", err, out)
	}
}

func writeFont(t *testing.T, f *apiFixture) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "go.ttf"), goregular.TTF, 0o600); err != nil {
		t.Fatal(err)
	}
	return ingestRID(t, f, "go.ttf", "intermediate")
}

func postJSON(t *testing.T, f *apiFixture, path, key string, body any) *http.Response {
	t.Helper()
	return postWithKey(t, f.ts.URL, path, body, testToken, key)
}

type cutResult struct {
	Segments []cutSegment `json:"segments"`
}

func decodeCut(t *testing.T, resp *http.Response) cutResult {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out cutResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeConcat(t *testing.T, resp *http.Response) concatResp {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		var b apiErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&b)
		t.Fatalf("status = %d %+v", resp.StatusCode, b.Error)
	}
	var out concatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertCheck(t *testing.T, resp *http.Response, status int, code, check string) {
	t.Helper()
	b := assertAPIError(t, resp, status, code)
	if b.Error.Check != check {
		t.Fatalf("check = %q, want %q", b.Error.Check, check)
	}
}

// ---- video/cut ----

func TestVideoCutTable(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 4, "160x120", 10, 44100)
	rid := ingestRID(t, f, "v.mp4", "intermediate")
	urlSrc := map[string]string{"url": f.origin.URL + "/v.mp4"}
	ridSrc := map[string]string{"rid": rid}
	seg := func(a, b float64) map[string]float64 { return map[string]float64{"start": a, "end": b} }

	okCases := []struct {
		name string
		segs []map[string]float64
		want []float64 // 各段期望时长
	}{
		{"两段", []map[string]float64{seg(0.5, 1.5), seg(2, 3.5)}, []float64{1, 1.5}},
		{"起点不在关键帧上", []map[string]float64{seg(1.7, 2.9)}, []float64{1.2}},
		{"毫秒精度", []map[string]float64{seg(0.123, 0.823)}, []float64{0.7}},
		{"整段", []map[string]float64{seg(0, 4)}, []float64{4}},
		{"重叠区间", []map[string]float64{seg(0, 2), seg(1.5, 3)}, []float64{2, 1.5}},
		{"结尾落在时长上", []map[string]float64{seg(3, 4)}, []float64{1}},
	}
	for i, c := range okCases {
		t.Run(c.name, func(t *testing.T) {
			out := decodeCut(t, postJSON(t, f, "/v1/mediasvc/video/cut", fmt.Sprintf("k-cut-ok-%d", i),
				map[string]any{"source": ridSrc, "segments": c.segs, "purpose": "intermediate"}))
			if len(out.Segments) != len(c.want) {
				t.Fatalf("段数 = %d", len(out.Segments))
			}
			seen := map[string]bool{}
			for j, sg := range out.Segments {
				if sg.Object.RID == "" || seen[sg.Object.RID] {
					t.Fatalf("段 %d rid 为空或重复: %q", j, sg.Object.RID)
				}
				seen[sg.Object.RID] = true
				if !near(sg.Object.DurationS, c.want[j], 0.2) {
					t.Fatalf("段 %d 时长 = %.3f, want %.3f", j, sg.Object.DurationS, c.want[j])
				}
				if sg.Object.Width != 160 || sg.Object.Height != 120 || !sg.Object.HasAudio {
					t.Fatalf("段 %d 对象 = %+v", j, sg.Object)
				}
			}
		})
	}

	errCases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
		check  string
	}{
		{"0 长度", map[string]any{"source": ridSrc, "segments": []any{seg(1, 1)}}, 400, "invalid_request", "request.segment_range"},
		{"end 小于 start", map[string]any{"source": ridSrc, "segments": []any{seg(2, 1)}}, 400, "invalid_request", "request.segment_range"},
		{"起点为负", map[string]any{"source": ridSrc, "segments": []any{seg(-1, 1)}}, 400, "invalid_request", "request.segment_range"},
		{"短于 0.1 秒", map[string]any{"source": ridSrc, "segments": []any{seg(1, 1.05)}}, 400, "invalid_request", "request.segment_too_short"},
		{"没有区间", map[string]any{"source": ridSrc, "segments": []any{}}, 400, "invalid_request", "request.segments_count"},
		{"超过 6 段", map[string]any{"source": ridSrc, "segments": []any{seg(0, 1), seg(0, 1), seg(0, 1), seg(0, 1), seg(0, 1), seg(0, 1), seg(0, 1)}}, 400, "invalid_request", "request.segments_count"},
		{"终点越界", map[string]any{"source": ridSrc, "segments": []any{seg(3, 4.5)}}, 422, "source_too_short", "video.cut_out_of_range"},
		{"起点越界", map[string]any{"source": ridSrc, "segments": []any{seg(4.5, 5.5)}}, 422, "source_too_short", "video.cut_out_of_range"},
		{"只接受 rid（url）", map[string]any{"source": urlSrc, "segments": []any{seg(0, 1)}}, 400, "invalid_request", "request.source_rid_only"},
		{"rid 格式不对", map[string]any{"source": map[string]string{"rid": "nope"}, "segments": []any{seg(0, 1)}}, 400, "invalid_request", "request.rid"},
	}
	for i, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			c.body["purpose"] = "intermediate"
			assertCheck(t, postJSON(t, f, "/v1/mediasvc/video/cut", fmt.Sprintf("k-cut-err-%d", i), c.body), c.status, c.code, c.check)
		})
	}

	t.Run("缺幂等键", func(t *testing.T) {
		resp := postJSON(t, f, "/v1/mediasvc/video/cut", "", map[string]any{"source": ridSrc, "segments": []any{seg(0, 1)}, "purpose": "intermediate"})
		assertCheck(t, resp, 400, "invalid_request", "request.idempotency_key_missing")
	})
}

func TestVideoCutRejectsOverLongSource(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "long.mp4"), 82, "64x64", 5, 0)
	rid := ingestRID(t, f, "long.mp4", "intermediate")
	resp := postJSON(t, f, "/v1/mediasvc/video/cut", "k-cut-long", map[string]any{
		"source": map[string]string{"rid": rid}, "segments": []any{map[string]float64{"start": 0, "end": 5}}, "purpose": "intermediate"})
	assertCheck(t, resp, 422, "source_rejected", "video.duration_exceeded")
}

func TestVideoCutIdempotentReplay(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 3, "160x120", 10, 0)
	rid := ingestRID(t, f, "v.mp4", "intermediate")
	body := map[string]any{"source": map[string]string{"rid": rid}, "purpose": "intermediate",
		"segments": []any{map[string]float64{"start": 0, "end": 1}, map[string]float64{"start": 1, "end": 2}}}
	a := rawJSON(t, postJSON(t, f, "/v1/mediasvc/video/cut", "k-cut-replay", body))
	resp := postJSON(t, f, "/v1/mediasvc/video/cut", "k-cut-replay", body)
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("同键同体应回放")
	}
	var m json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if string(a) != string(m) {
		t.Fatalf("回放内容不一致:\n%s\n%s", a, m)
	}
	// 同键异体：400
	body["segments"] = []any{map[string]float64{"start": 0, "end": 2}}
	assertCheck(t, postJSON(t, f, "/v1/mediasvc/video/cut", "k-cut-replay", body), 400, "invalid_request", "request.idempotency_key_reused")
	// 换键同体：同一输入重新执行，各段 rid 随键变化（rid 由键派生）
	body["segments"] = []any{map[string]float64{"start": 0, "end": 1}, map[string]float64{"start": 1, "end": 2}}
	first := decodeCut(t, postJSON(t, f, "/v1/mediasvc/video/cut", "k-cut-replay-2", body))
	var orig cutResult
	_ = json.Unmarshal(a, &orig)
	if first.Segments[0].Object.RID == orig.Segments[0].Object.RID {
		t.Fatal("不同幂等键应得到不同 rid")
	}
}

// ---- video/concat ----

func TestVideoConcatTable(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "a.mp4"), 2, "160x120", 10, 44100) // 基准规格，有声
	makeSpecClip(t, filepath.Join(f.dir, "b.mp4"), 1.5, "100x200", 15, 0)   // 规格不同，无声
	makeSpecClip(t, filepath.Join(f.dir, "c.mp4"), 1, "120x120", 24, 22050) // 规格不同，有声，采样率不同
	a := ingestRID(t, f, "a.mp4", "intermediate")
	b := ingestRID(t, f, "b.mp4", "intermediate")
	c := ingestRID(t, f, "c.mp4", "intermediate")
	font := writeFont(t, f)
	src := func(rid string) map[string]any { return map[string]any{"source": map[string]string{"rid": rid}} }
	ph := func(d float64, text string) map[string]any {
		return map[string]any{"placeholder": map[string]any{"duration": d, "text": text, "font_rid": font}}
	}

	okCases := []struct {
		name         string
		body         map[string]any
		wantDur      float64
		wantW, wantH int
		wantAudio    bool
		wantFps      float64
		wantKinds    []string
	}{
		{"单段拼接", map[string]any{"items": []any{src(a)}}, 2, 160, 120, true, 10, []string{"source"}},
		{"规格不一致统一到第一段", map[string]any{"items": []any{src(a), src(b), src(c)}}, 4.5, 160, 120, true, 10, []string{"source", "source", "source"}},
		{"去除音轨", map[string]any{"items": []any{src(a), src(c)}, "audio": "drop"}, 3, 160, 120, false, 10, []string{"source", "source"}},
		{"参数指定规格", map[string]any{"items": []any{src(a), src(b)}, "width": 200, "height": 100, "fps": 20, "sample_rate": 48000}, 3.5, 200, 100, true, 20, []string{"source", "source"}},
		{"缺失段占位", map[string]any{"items": []any{src(a), ph(1.5, "Redo this part"), src(c)}}, 4.5, 160, 120, true, 10, []string{"source", "placeholder", "source"}},
		{"占位在最前", map[string]any{"items": []any{ph(1, "Redo"), src(b)}}, 2.5, 100, 200, false, 15, []string{"placeholder", "source"}},
		{"全是占位", map[string]any{"items": []any{ph(1, "Redo")}, "width": 128, "height": 72}, 1, 128, 72, false, 25, []string{"placeholder"}},
		{"占位无文字", map[string]any{"items": []any{src(a), map[string]any{"placeholder": map[string]any{"duration": 0.5}}}}, 2.5, 160, 120, true, 10, []string{"source", "placeholder"}},
	}
	for i, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["purpose"] = "intermediate"
			out := decodeConcat(t, postJSON(t, f, "/v1/mediasvc/video/concat", fmt.Sprintf("k-cc-ok-%d", i), tc.body))
			o := out.Object
			if o.RID == "" || !near(o.DurationS, tc.wantDur, 0.35) || o.Width != tc.wantW || o.Height != tc.wantH || o.HasAudio != tc.wantAudio || !near(o.Fps, tc.wantFps, 0.5) {
				t.Fatalf("object = %+v, want dur %.2f %dx%d audio=%v fps=%.0f", o, tc.wantDur, tc.wantW, tc.wantH, tc.wantAudio, tc.wantFps)
			}
			if len(out.Timeline) != len(tc.wantKinds) {
				t.Fatalf("timeline = %+v", out.Timeline)
			}
			at := 0.0
			for j, e := range out.Timeline {
				if e.Index != j || e.Kind != tc.wantKinds[j] || !near(e.Start, at, 0.001) {
					t.Fatalf("timeline[%d] = %+v, want kind %s start %.3f", j, e, tc.wantKinds[j], at)
				}
				at += e.Duration
			}
		})
	}

	errCases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
		check  string
	}{
		{"没有段", map[string]any{"items": []any{}}, 400, "invalid_request", "request.segments_count"},
		{"超过 6 段", map[string]any{"items": []any{src(a), src(a), src(a), src(a), src(a), src(a), src(a)}}, 400, "invalid_request", "request.segments_count"},
		{"段既无源也无占位", map[string]any{"items": []any{map[string]any{}}}, 400, "invalid_request", "request.item_exactly_one"},
		{"段同时有源和占位", map[string]any{"items": []any{map[string]any{"source": map[string]string{"rid": a}, "placeholder": map[string]any{"duration": 1}}}}, 400, "invalid_request", "request.item_exactly_one"},
		{"只接受 rid", map[string]any{"items": []any{map[string]any{"source": map[string]string{"url": f.origin.URL + "/a.mp4"}}}}, 400, "invalid_request", "request.source_rid_only"},
		{"占位时长为 0", map[string]any{"items": []any{src(a), ph(0, "x")}}, 400, "invalid_request", "request.placeholder_duration"},
		{"全占位且没给尺寸", map[string]any{"items": []any{ph(1, "x")}}, 400, "invalid_request", "request.placeholder_needs_size"},
		{"宽高只给一个", map[string]any{"items": []any{src(a)}, "width": 100}, 400, "invalid_request", "request.size_pair"},
		{"宽高越界", map[string]any{"items": []any{src(a)}, "width": 8000, "height": 100}, 400, "invalid_request", "request.size_range"},
		{"帧率越界", map[string]any{"items": []any{src(a)}, "fps": 120}, 400, "invalid_request", "request.fps_range"},
		{"音轨模式非法", map[string]any{"items": []any{src(a)}, "audio": "mute"}, 400, "invalid_request", "request.audio_mode"},
		{"中文占位没给字体", map[string]any{"items": []any{src(a), map[string]any{"placeholder": map[string]any{"duration": 1, "text": "这一段还在重做"}}}}, 400, "invalid_request", "text.font_required"},
		{"字体缺字形", map[string]any{"items": []any{src(a), map[string]any{"placeholder": map[string]any{"duration": 1, "text": "这一段还在重做", "font_rid": font}}}}, 422, "source_rejected", "text.glyph_missing"},
	}
	for i, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["purpose"] = "intermediate"
			assertCheck(t, postJSON(t, f, "/v1/mediasvc/video/concat", fmt.Sprintf("k-cc-err-%d", i), tc.body), tc.status, tc.code, tc.check)
		})
	}
}

func TestVideoConcatRejectsTotalOverLimit(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "l.mp4"), 50, "64x64", 5, 0)
	rid := ingestRID(t, f, "l.mp4", "intermediate")
	it := map[string]any{"source": map[string]string{"rid": rid}}
	resp := postJSON(t, f, "/v1/mediasvc/video/concat", "k-cc-long", map[string]any{"items": []any{it, it}, "purpose": "intermediate"})
	assertCheck(t, resp, 422, "source_rejected", "video.total_duration_exceeded")
}

func TestVideoConcatIdempotentReplay(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "a.mp4"), 1, "160x120", 10, 44100)
	rid := ingestRID(t, f, "a.mp4", "intermediate")
	body := map[string]any{"items": []any{map[string]any{"source": map[string]string{"rid": rid}}, map[string]any{"placeholder": map[string]any{"duration": 0.5}}}, "purpose": "intermediate"}
	first := decodeConcat(t, postJSON(t, f, "/v1/mediasvc/video/concat", "k-cc-replay", body))
	resp := postJSON(t, f, "/v1/mediasvc/video/concat", "k-cc-replay", body)
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("同键同体应回放")
	}
	var again concatResp
	_ = json.NewDecoder(resp.Body).Decode(&again)
	if again.Object.RID != first.Object.RID || len(again.Timeline) != 2 {
		t.Fatalf("回放结果不一致: %+v vs %+v", again, first)
	}
}

// ---- 文字叠加 ----

func loadPNG(t *testing.T, path string) image.Image {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	img, err := png.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// regionDiff 两张图在矩形内「差异明显」的像素数。
func regionDiff(a, b image.Image, r image.Rectangle) int {
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			ar, ag, ab, _ := a.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			d := func(p, q uint32) int {
				if p > q {
					return int(p-q) >> 8
				}
				return int(q-p) >> 8
			}
			if d(ar, br) > 60 || d(ag, bg) > 60 || d(ab, bb) > 60 {
				n++
			}
		}
	}
	return n
}

func frameAt(t *testing.T, video string, sec float64, out string) image.Image {
	t.Helper()
	if o, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-ss", strconv.FormatFloat(sec, 'f', 2, 64), "-i", video,
		"-frames:v", "1", out).CombinedOutput(); err != nil {
		t.Fatalf("抽帧失败: %v: %s", err, o)
	}
	return loadPNG(t, out)
}

func TestImageTextDrawsInsideBoxOnly(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "bg.png"), 320, 240)
	img := ingestRID(t, f, "bg.png", "intermediate")
	font := writeFont(t, f)
	slot := map[string]any{"text": "Hello text slot", "font_rid": font, "box": map[string]float64{"x": 0.25, "y": 0.5, "w": 0.5, "h": 0.25},
		"color": "#FF0000", "stroke": map[string]any{"color": "#000000", "width": 2}, "shadow": map[string]any{}}
	res := decodeMediaResult(t, postJSON(t, f, "/v1/mediasvc/image/text", "k-it-1",
		map[string]any{"source": map[string]string{"rid": img}, "slots": []any{slot}, "purpose": "intermediate"}))
	if res.Object.Width != 320 || res.Object.Height != 240 {
		t.Fatalf("尺寸变了: %+v", res.Object)
	}
	orig := loadPNG(t, filepath.Join(f.dir, "bg.png"))
	got := loadPNG(t, storedObjectPath(t, f.objects, res))
	box := image.Rect(80, 120, 240, 180)
	if n := regionDiff(orig, got, box); n < 100 {
		t.Fatalf("框内应有文字，差异像素 = %d", n)
	}
	// 框外一个像素都不能动
	for _, r := range []image.Rectangle{image.Rect(0, 0, 320, 120), image.Rect(0, 180, 320, 240), image.Rect(0, 120, 80, 180), image.Rect(240, 120, 320, 180)} {
		if n := regionDiff(orig, got, r); n != 0 {
			t.Fatalf("文字溢出到框外 %v，差异像素 = %d", r, n)
		}
	}
}

func TestImageTextJPEGKeepsFormat(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=200x100:rate=1:duration=1",
		"-frames:v", "1", filepath.Join(f.dir, "p.jpg")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	img := ingestRID(t, f, "p.jpg", "intermediate")
	res := decodeMediaResult(t, postJSON(t, f, "/v1/mediasvc/image/text", "k-it-jpg", map[string]any{
		"source": map[string]string{"rid": img}, "purpose": "intermediate",
		"slots": []any{map[string]any{"text": "Sale", "box": map[string]float64{"x": 0, "y": 0, "w": 1, "h": 0.5}}}}))
	if res.Object.Mime != "image/jpeg" || res.Object.Width != 200 {
		t.Fatalf("object = %+v", res.Object)
	}
}

func TestImageTextErrorsAndLimits(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "bg.png"), 160, 120)
	img := ingestRID(t, f, "bg.png", "intermediate")
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 1, "64x64", 5, 0)
	vid := ingestRID(t, f, "v.mp4", "intermediate")
	font := writeFont(t, f)
	box := map[string]float64{"x": 0.1, "y": 0.1, "w": 0.8, "h": 0.3}
	slot := func(m map[string]any) []any {
		s := map[string]any{"text": "ok", "box": box}
		for k, v := range m {
			s[k] = v
		}
		return []any{s}
	}
	many := make([]any, 17)
	for i := range many {
		many[i] = map[string]any{"text": "x", "box": box}
	}
	cases := []struct {
		name   string
		source any
		slots  any
		status int
		code   string
		check  string
	}{
		{"url 来源", map[string]string{"url": f.origin.URL + "/bg.png"}, slot(nil), 400, "invalid_request", "request.source_rid_only"},
		{"不是图片", map[string]string{"rid": vid}, slot(nil), 422, "source_rejected", "image.not_image"},
		{"没有槽位", map[string]string{"rid": img}, []any{}, 400, "invalid_request", "request.slots_count"},
		{"17 个槽位", map[string]string{"rid": img}, many, 400, "invalid_request", "request.slots_count"},
		{"文字为空", map[string]string{"rid": img}, slot(map[string]any{"text": "  "}), 400, "invalid_request", "request.slot_text_empty"},
		{"文字超长", map[string]string{"rid": img}, slot(map[string]any{"text": strings.Repeat("a", 501)}), 400, "invalid_request", "request.slot_text"},
		{"框越界", map[string]string{"rid": img}, slot(map[string]any{"box": map[string]float64{"x": 0.5, "y": 0, "w": 0.6, "h": 0.5}}), 400, "invalid_request", "request.slot_box_range"},
		{"框宽为 0", map[string]string{"rid": img}, slot(map[string]any{"box": map[string]float64{"x": 0, "y": 0, "w": 0, "h": 0.5}}), 400, "invalid_request", "request.slot_box_range"},
		{"颜色非法", map[string]string{"rid": img}, slot(map[string]any{"color": "red"}), 400, "invalid_request", "request.slot_color"},
		{"对齐非法", map[string]string{"rid": img}, slot(map[string]any{"align": "justify"}), 400, "invalid_request", "request.slot_align"},
		{"图片不能带时间", map[string]string{"rid": img}, slot(map[string]any{"start": 0, "end": 1}), 400, "invalid_request", "request.slot_time_unsupported"},
		{"字体不是字体", map[string]string{"rid": img}, slot(map[string]any{"font_rid": vid}), 422, "source_rejected", "font.invalid"},
		{"中文没给字体", map[string]string{"rid": img}, slot(map[string]any{"text": "店名"}), 400, "invalid_request", "text.font_required"},
		{"字体缺字形", map[string]string{"rid": img}, slot(map[string]any{"text": "店名", "font_rid": font}), 422, "source_rejected", "text.glyph_missing"},
		{"框小到放不下", map[string]string{"rid": img}, slot(map[string]any{"box": map[string]float64{"x": 0, "y": 0, "w": 0.005, "h": 0.005}}), 400, "invalid_request", "request.slot_box_too_small"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postJSON(t, f, "/v1/mediasvc/image/text", fmt.Sprintf("k-it-err-%d", i),
				map[string]any{"source": c.source, "slots": c.slots, "purpose": "intermediate"})
			assertCheck(t, resp, c.status, c.code, c.check)
		})
	}

	t.Run("超长文字省略且不溢出", func(t *testing.T) {
		long := strings.Repeat("overflow ", 200)[:490]
		res := decodeMediaResult(t, postJSON(t, f, "/v1/mediasvc/image/text", "k-it-long", map[string]any{
			"source": map[string]string{"rid": img}, "purpose": "intermediate",
			"slots": slot(map[string]any{"text": long, "font_rid": font, "size": 20, "max_lines": 2})}))
		orig := loadPNG(t, filepath.Join(f.dir, "bg.png"))
		got := loadPNG(t, storedObjectPath(t, f.objects, res))
		if n := regionDiff(orig, got, image.Rect(16, 12, 144, 48)); n == 0 {
			t.Fatal("框内应有文字")
		}
		if n := regionDiff(orig, got, image.Rect(0, 49, 160, 120)); n != 0 {
			t.Fatalf("文字溢出到框下方，差异像素 = %d", n)
		}
	})

	t.Run("幂等重放", func(t *testing.T) {
		f := newCopyFixture(t)
		makeImage(t, filepath.Join(f.dir, "bg.png"), 160, 120)
		img := ingestRID(t, f, "bg.png", "intermediate")
		body := map[string]any{"source": map[string]string{"rid": img}, "slots": slot(nil), "purpose": "intermediate"}
		a := decodeMediaResult(t, postJSON(t, f, "/v1/mediasvc/image/text", "k-it-replay", body))
		resp := postJSON(t, f, "/v1/mediasvc/image/text", "k-it-replay", body)
		b := decodeMediaResult(t, resp)
		if resp.Header.Get("Idempotent-Replayed") != "true" || a.Object.RID != b.Object.RID {
			t.Fatalf("重放不一致: %s vs %s", a.Object.RID, b.Object.RID)
		}
	})
}

func TestVideoOverlayTextTimedSlot(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 3, "320x240", 10, 44100)
	vid := ingestRID(t, f, "v.mp4", "intermediate")
	font := writeFont(t, f)
	slot := map[string]any{"text": "Open 9-21", "font_rid": font, "box": map[string]float64{"x": 0.25, "y": 0.5, "w": 0.5, "h": 0.25},
		"color": "#FF0000", "start": 1.0, "end": 2.0}
	res := decodeMediaResult(t, postJSON(t, f, "/v1/mediasvc/video/overlay-text", "k-vo-1",
		map[string]any{"source": map[string]string{"rid": vid}, "slots": []any{slot}, "purpose": "intermediate"}))
	if res.Object.Width != 320 || res.Object.Height != 240 || !res.Object.HasAudio || !near(res.Object.DurationS, 3, 0.3) {
		t.Fatalf("object = %+v", res.Object)
	}
	out := storedObjectPath(t, f.objects, res)
	src := filepath.Join(f.dir, "v.mp4")
	tmp := t.TempDir()
	box := image.Rect(80, 120, 240, 180)
	inside := regionDiff(frameAt(t, src, 1.5, filepath.Join(tmp, "a0.png")), frameAt(t, out, 1.5, filepath.Join(tmp, "a1.png")), box)
	before := regionDiff(frameAt(t, src, 0.3, filepath.Join(tmp, "b0.png")), frameAt(t, out, 0.3, filepath.Join(tmp, "b1.png")), box)
	after := regionDiff(frameAt(t, src, 2.6, filepath.Join(tmp, "c0.png")), frameAt(t, out, 2.6, filepath.Join(tmp, "c1.png")), box)
	if inside < 100 || before > inside/10 || after > inside/10 {
		t.Fatalf("分时显示不对：窗口内 %d，之前 %d，之后 %d", inside, before, after)
	}
}

func TestVideoOverlayTextErrors(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 2, "160x120", 10, 0)
	vid := ingestRID(t, f, "v.mp4", "intermediate")
	box := map[string]float64{"x": 0.1, "y": 0.1, "w": 0.8, "h": 0.3}
	mk := func(m map[string]any) []any {
		s := map[string]any{"text": "ok", "box": box}
		for k, v := range m {
			s[k] = v
		}
		return []any{s}
	}
	cases := []struct {
		name   string
		slots  []any
		status int
		code   string
		check  string
	}{
		{"只给 start", mk(map[string]any{"start": 1}), 400, "invalid_request", "request.slot_time_range"},
		{"end 不大于 start", mk(map[string]any{"start": 1, "end": 1}), 400, "invalid_request", "request.slot_time_range"},
		{"start 在片尾之后", mk(map[string]any{"start": 5, "end": 6}), 400, "invalid_request", "request.slot_time_range"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postJSON(t, f, "/v1/mediasvc/video/overlay-text", fmt.Sprintf("k-vo-err-%d", i),
				map[string]any{"source": map[string]string{"rid": vid}, "slots": c.slots, "purpose": "intermediate"})
			assertCheck(t, resp, c.status, c.code, c.check)
		})
	}
	t.Run("url 来源", func(t *testing.T) {
		resp := postJSON(t, f, "/v1/mediasvc/video/overlay-text", "k-vo-url",
			map[string]any{"source": map[string]string{"url": f.origin.URL + "/v.mp4"}, "slots": mk(nil), "purpose": "intermediate"})
		assertCheck(t, resp, 400, "invalid_request", "request.source_rid_only")
	})
	t.Run("end 超出片尾自动截到片尾", func(t *testing.T) {
		resp := postJSON(t, f, "/v1/mediasvc/video/overlay-text", "k-vo-clamp",
			map[string]any{"source": map[string]string{"rid": vid}, "slots": mk(map[string]any{"start": 1, "end": 99}), "purpose": "intermediate"})
		if res := decodeMediaResult(t, resp); res.Object.RID == "" {
			t.Fatal("应成功")
		}
	})
}

func TestReplicateCapabilitiesRegistered(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mediasvc/capabilities", nil)
	resp := send(t, req, testToken)
	var caps map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&caps)
	for _, k := range []string{"video_cut", "video_concat", "video_concat_placeholder", "video_overlay_text", "image_text"} {
		if !caps[k] {
			t.Fatalf("capabilities 缺 %s: %v", k, caps)
		}
	}
}
