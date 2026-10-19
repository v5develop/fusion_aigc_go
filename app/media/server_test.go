package media

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "test-token"

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("未安装 %s，跳过", b)
		}
	}
}

// makeClip 用 ffmpeg 生成测试视频（带音轨与否由 withAudio 决定）。
func makeClip(t *testing.T, path string, seconds string, withAudio bool) {
	t.Helper()
	args := []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=" + seconds + ":size=160x120:rate=10"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration="+seconds)
	}
	args = append(args, "-pix_fmt", "yuv420p", "-shortest", path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v: %s", err, out)
	}
}

// fileServer 把目录当作源站，供服务下载。
func fileServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.Token = testToken
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 2
	}
	if cfg.MaxInputBytes == 0 {
		cfg.MaxInputBytes = 50 << 20
	}
	if cfg.MaxComposeInputs == 0 {
		cfg.MaxComposeInputs = 5
	}
	if cfg.FetchTimeoutSeconds == 0 {
		cfg.FetchTimeoutSeconds = 60
	}
	if cfg.OpTimeoutSeconds == 0 {
		cfg.OpTimeoutSeconds = 120
	}
	cfg.TempDir = t.TempDir()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, base, path string, body any, token string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return send(t, req, token)
}

// postFile 把视频文件作为请求体直接发过去（Java 本地临时文件走这条路）。
func postFile(t *testing.T, base, path, file, token string) *http.Response {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "video/mp4")
	return send(t, req, token)
}

func send(t *testing.T, req *http.Request, token string) *http.Response {
	t.Helper()
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func assertError(t *testing.T, resp *http.Response, status int, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d", resp.StatusCode, status)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["code"] != code {
		t.Fatalf("code = %q, want %q (body %v)", body["code"], code, body)
	}
}

func TestRejectsMissingOrWrongToken(t *testing.T) {
	ts := newTestServer(t, Config{})
	assertError(t, post(t, ts.URL, "/v1/cover", videoRequest{VideoURL: "http://x/a.mp4"}, ""), http.StatusUnauthorized, "unauthorized")
	assertError(t, post(t, ts.URL, "/v1/cover", videoRequest{VideoURL: "http://x/a.mp4"}, "wrong"), http.StatusUnauthorized, "unauthorized")
}

func TestNewServerRefusesEmptyToken(t *testing.T) {
	if _, err := NewServer(Config{}); err == nil {
		t.Fatal("没有 Token 必须拒绝启动")
	}
}

func TestRejectsNonHTTPScheme(t *testing.T) {
	ts := newTestServer(t, Config{})
	assertError(t, post(t, ts.URL, "/v1/cover", videoRequest{VideoURL: "file:///etc/passwd"}, testToken),
		http.StatusUnprocessableEntity, "bad_input")
}

// 默认禁止内网 / 回环地址：源站就在本机也必须被拦住（防 SSRF）。
func TestBlocksLoopbackByDefault(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(src, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{})
	assertError(t, post(t, ts.URL, "/v1/cover", videoRequest{VideoURL: origin.URL + "/a.mp4"}, testToken),
		http.StatusUnprocessableEntity, "bad_input")
}

func TestCoverAcceptsRawBody(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeClip(t, clip, "4", false)
	ts := newTestServer(t, Config{}) // 不开私网访问：请求体模式不需要下载，不受 SSRF 限制

	resp := postFile(t, ts.URL, "/v1/cover", clip, testToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestCoverRejectsEmptyBody(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/cover", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "video/mp4")
	assertError(t, send(t, req, testToken), http.StatusUnprocessableEntity, "bad_input")
}

func TestCoverReturnsJPEG(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "a.mp4"), "4", false)
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true})

	resp := post(t, ts.URL, "/v1/cover", videoRequest{VideoURL: origin.URL + "/a.mp4"}, testToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !bytes.HasPrefix(buf.Bytes(), []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatal("封面不是 JPEG")
	}
}

func TestComposeConcatenatesClips(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "a.mp4"), "2", true)
	makeClip(t, filepath.Join(dir, "b.mp4"), "3", true)
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true})

	resp := post(t, ts.URL, "/v1/compose", composeRequest{VideoURLs: []string{
		origin.URL + "/a.mp4", origin.URL + "/b.mp4",
	}}, testToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := filepath.Join(dir, "out.mp4")
	f, _ := os.Create(out)
	_, _ = f.ReadFrom(resp.Body)
	f.Close()

	// 2s + 3s：拼接后时长约 5 秒
	got, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", out).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(got)), "4.") && !strings.HasPrefix(strings.TrimSpace(string(got)), "5.") {
		t.Fatalf("合成时长 = %s，期望约 5 秒", got)
	}
}

func TestComposeFallsBackWhenAudioMissing(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "a.mp4"), "2", true)
	makeClip(t, filepath.Join(dir, "b.mp4"), "2", false) // 无音轨：走仅视频的 filter 分支
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true})

	resp := post(t, ts.URL, "/v1/compose", composeRequest{VideoURLs: []string{
		origin.URL + "/a.mp4", origin.URL + "/b.mp4",
	}}, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无音轨输入应当仍然合成成功，status=%d", resp.StatusCode)
	}
}

func TestComposeRejectsTooManyInputs(t *testing.T) {
	ts := newTestServer(t, Config{MaxComposeInputs: 2})
	assertError(t, post(t, ts.URL, "/v1/compose", composeRequest{VideoURLs: []string{
		"http://a/1.mp4", "http://a/2.mp4", "http://a/3.mp4",
	}}, testToken), http.StatusUnprocessableEntity, "bad_input")
}

func TestWatermarkNeedsLogo(t *testing.T) {
	ts := newTestServer(t, Config{})
	assertError(t, post(t, ts.URL, "/v1/watermark", videoRequest{VideoURL: "http://a/1.mp4"}, testToken),
		http.StatusConflict, "logo_unavailable")
}

func TestWatermarkOverlaysLogo(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "a.mp4"), "2", true)
	logo := filepath.Join(dir, "logo.png")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=red:s=32x32",
		"-frames:v", "1", logo).CombinedOutput(); err != nil {
		t.Fatalf("生成 logo 失败: %v: %s", err, out)
	}
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, LogoPath: logo})

	resp := post(t, ts.URL, "/v1/watermark", videoRequest{VideoURL: origin.URL + "/a.mp4"}, testToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestCapabilitiesReportsWatermarkReadiness(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/capabilities", nil)
	req.Header.Set(TokenHeader, testToken)
	resp := send(t, req, testToken)
	var caps map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&caps)
	if resp.StatusCode != http.StatusOK || caps["watermark"] || !caps["cover"] {
		t.Fatalf("status=%d caps=%v（未配 logo 时 watermark 应为 false）", resp.StatusCode, caps)
	}
}
