package media

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---- 纯逻辑：与原 Java 实现的输出逐字一致 ----

func TestTimelineFilterMatchesJavaOutput(t *testing.T) {
	got, err := TimelineFilter([]float64{0, 1.5}, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := "[0:a]adelay=0:all=1[a0];[1:a]adelay=1500:all=1[a1];[a0][a1]amix=inputs=2:normalize=0,apad,atrim=duration=4,asetpts=N/SR/TB[aout]"
	if got != want {
		t.Fatalf("filter =\n%s\nwant\n%s", got, want)
	}
	if _, err := TimelineFilter(nil, 4); err == nil {
		t.Fatal("空片段必须报错")
	}
}

func TestFFmpegNumberTrimsLikeJava(t *testing.T) {
	cases := map[float64]string{1.5: "1.5", 4: "4", 0.3333333333: "0.333333", 12.250000: "12.25"}
	for in, want := range cases {
		if got := ffmpegNumber(in); got != want {
			t.Errorf("ffmpegNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFrameTimestampsMatchJavaComputeTimestamps(t *testing.T) {
	if got := FrameTimestamps(10, 3); !equalFloats(got, []float64{1.7, 5, 8.3}) {
		t.Fatalf("FrameTimestamps(10,3) = %v", got)
	}
	if got := FrameTimestamps(-1, 5); !equalFloats(got, []float64{0}) {
		t.Fatalf("未知时长应退化为单帧 0，得到 %v", got)
	}
	// 时长很短时相邻采样点会落到同一个 0.1 格上，必须去重
	if got := FrameTimestamps(0.1, 5); len(got) >= 5 {
		t.Fatalf("相邻采样点没去重: %v", got)
	}
}

func TestParseFPS(t *testing.T) {
	if got := parseFPS("30000/1001"); math.Abs(got-29.97) > 0.01 {
		t.Errorf("30000/1001 -> %v", got)
	}
	if parseFPS("25/1") != 25 || parseFPS("0/0") != 0 || parseFPS("") != 0 || parseFPS("abc") != 0 {
		t.Error("帧率解析不对")
	}
}

func TestParseProbeReadsFirstStreams(t *testing.T) {
	raw := []byte(`{"format":{"duration":"4.000000"},"streams":[
		{"codec_type":"audio","sample_rate":"44100","channels":2},
		{"codec_type":"video","width":1280,"height":720,"avg_frame_rate":"25/1"}]}`)
	p, err := parseProbe(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 1280 || p.Height != 720 || p.DurationSeconds != 4 || p.FPS != 25 ||
		!p.HasVideo || !p.HasAudio || p.SampleRate != 44100 || p.Channels != 2 {
		t.Fatalf("probe = %+v", p)
	}
	if _, err := parseProbe([]byte("not json")); err == nil {
		t.Fatal("非 JSON 必须报错")
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

// ---- 真实 ffmpeg ----

// makeAVClip 生成 4 秒的音视频测试片。
func makeAVClip(t *testing.T, path string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=4:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-pix_fmt", "yuv420p", "-shortest", path).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v: %s", err, out)
	}
}

func durationOf(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("时长解析: %v", err)
	}
	return d
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// hasSubtitlesFilter 烧字幕依赖 libass。没有的本机 ffmpeg 跳过这部分；容器镜像里是有的。
func hasSubtitlesFilter() bool {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output()
	return err == nil && strings.Contains(string(out), " subtitles ")
}

func newAVService(t *testing.T) *Service {
	t.Helper()
	return NewService(Config{MaxConcurrent: 2, MaxInputBytes: 50 << 20, MaxComposeInputs: 5,
		OpTimeoutSeconds: 120, FetchTimeoutSeconds: 60, TempDir: t.TempDir(), FFmpegThreads: 1})
}

func TestProbeRealClip(t *testing.T) {
	requireFFmpeg(t)
	clip := filepath.Join(t.TempDir(), "a.mp4")
	makeAVClip(t, clip)
	p, err := newAVService(t).Probe(context.Background(), clip)
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 320 || p.Height != 240 || !p.HasVideo || !p.HasAudio || !near(p.DurationSeconds, 4, 0.2) {
		t.Fatalf("probe = %+v", p)
	}
}

func TestFramesReturnJPEGs(t *testing.T) {
	requireFFmpeg(t)
	clip := filepath.Join(t.TempDir(), "a.mp4")
	makeAVClip(t, clip)
	frames, err := newAVService(t).Frames(context.Background(), clip, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("抽了 %d 帧，want 3", len(frames))
	}
	for _, f := range frames {
		if !bytes.HasPrefix(f.JPEG, []byte{0xFF, 0xD8, 0xFF}) {
			t.Fatal("抽帧不是 JPEG")
		}
	}
}

func TestValidateAcceptsGoodAndRejectsGarbage(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	svc := newAVService(t)
	if err := svc.Validate(context.Background(), clip); err != nil {
		t.Fatalf("好视频应当通过: %v", err)
	}
	bad := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(bad, []byte("definitely not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Validate(context.Background(), bad); err == nil {
		t.Fatal("坏文件必须被拒绝")
	}
}

func TestAudioOperationsEndToEnd(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	svc := newAVService(t)
	ctx := context.Background()

	mp3 := filepath.Join(dir, "audio.mp3")
	if err := svc.ExtractAudio(ctx, clip, mp3); err != nil {
		t.Fatal(err)
	}
	if !near(durationOf(t, mp3), 4, 0.2) {
		t.Fatal("抽出的音轨时长不对")
	}

	m4a := filepath.Join(dir, "timeline.m4a")
	if err := svc.AudioTimeline(ctx, []string{mp3, mp3}, []float64{0, 1}, 4, 44100, 1, m4a); err != nil {
		t.Fatal(err)
	}
	if !near(durationOf(t, m4a), 4, 0.2) {
		t.Fatalf("音轨拼接时长 = %v，期望 4", durationOf(t, m4a))
	}

	fast := filepath.Join(dir, "fast.mp4")
	if err := svc.AdjustDuration(ctx, clip, 2, fast); err != nil {
		t.Fatal(err)
	}
	if !near(durationOf(t, fast), 8, 0.4) {
		t.Fatalf("变速后时长 = %v，期望约 8", durationOf(t, fast))
	}

	replaced := filepath.Join(dir, "replaced.mp4")
	if err := svc.ReplaceAudio(ctx, fast, m4a, replaced); err != nil {
		t.Fatal(err)
	}
	// 与原 Java 的 -shortest 语义一致：输出按短的一方截断（音轨 4 秒，视频 8 秒 → 4 秒）
	if !near(durationOf(t, replaced), 4, 0.3) {
		t.Fatalf("换音轨后时长 = %v，期望按短的音轨截断为 4", durationOf(t, replaced))
	}

	if !hasSubtitlesFilter() {
		t.Log("跳过烧字幕：本机 ffmpeg 没有 subtitles 滤镜（需要 libass，容器镜像里有）")
		return
	}
	srt := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:02,000\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	burned := filepath.Join(dir, "burned.mp4")
	if err := svc.BurnSubtitles(ctx, dir, clip, srt, burned); err != nil {
		t.Fatalf("烧字幕失败: %v", err)
	}
	if !near(durationOf(t, burned), 4, 0.3) {
		t.Fatal("烧字幕后时长变了")
	}
}

// ---- HTTP：multipart 与原始体 ----

func TestAVHTTPRoutes(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	ts := newTestServer(t, Config{})

	// 探测：原始体
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/probe", bytes.NewReader(mustRead(t, clip)))
	req.Header.Set("Content-Type", "video/mp4")
	resp := send(t, req, testToken)
	var probe ProbeResult
	_ = json.NewDecoder(resp.Body).Decode(&probe)
	if resp.StatusCode != http.StatusOK || probe.Width != 320 {
		t.Fatalf("probe status=%d %+v", resp.StatusCode, probe)
	}

	// 抽帧：count 越界
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/frames?count=99", bytes.NewReader(mustRead(t, clip)))
	req.Header.Set("Content-Type", "video/mp4")
	assertError(t, send(t, req, testToken), http.StatusUnprocessableEntity, "bad_input")

	// 音轨拼接：multipart，params + audio0/audio1
	mp3 := filepath.Join(dir, "audio.mp3")
	if err := newAVService(t).ExtractAudio(context.Background(), clip, mp3); err != nil {
		t.Fatal(err)
	}
	body, ctype := multipartBody(t, map[string]string{
		"params": `{"starts":[0,1],"total_duration":4,"sample_rate":44100,"channels":1}`,
	}, map[string]string{"audio0": mp3, "audio1": mp3})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/audio/timeline", body)
	req.Header.Set("Content-Type", ctype)
	resp = send(t, req, testToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("timeline status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// 缺文件：明确报错而不是 500
	body, ctype = multipartBody(t, map[string]string{
		"params": `{"starts":[0,1],"total_duration":4}`,
	}, map[string]string{"audio0": mp3})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/audio/timeline", body)
	req.Header.Set("Content-Type", ctype)
	assertError(t, send(t, req, testToken), http.StatusUnprocessableEntity, "bad_input")

	// 替换音轨：multipart video + audio
	body, ctype = multipartBody(t, nil, map[string]string{"video": clip, "audio": mp3})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/video/replace-audio", body)
	req.Header.Set("Content-Type", ctype)
	resp = send(t, req, testToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("replace status=%d", resp.StatusCode)
	}

	// 字幕：multipart video + subtitle
	if !hasSubtitlesFilter() {
		t.Log("跳过字幕路由：本机 ffmpeg 没有 subtitles 滤镜")
		return
	}
	srt := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:02,000\nhi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, ctype = multipartBody(t, nil, map[string]string{"video": clip, "subtitle": srt})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/video/burn-subtitles", body)
	req.Header.Set("Content-Type", ctype)
	resp = send(t, req, testToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("burn status=%d body=%s", resp.StatusCode, b)
	}

	// 字段名含路径字符必须拒绝
	body, ctype = multipartBody(t, nil, map[string]string{"../evil": clip})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/video/replace-audio", body)
	req.Header.Set("Content-Type", ctype)
	assertError(t, send(t, req, testToken), http.StatusUnprocessableEntity, "bad_input")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// multipartBody 组装 multipart：fields 是普通字段，files 是 字段名 → 本地文件路径。
func multipartBody(t *testing.T, fields map[string]string, files map[string]string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range files {
		part, err := w.CreateFormFile(name, filepath.Base(path))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}
