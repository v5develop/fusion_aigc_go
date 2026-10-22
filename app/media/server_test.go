package media

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// requireFFmpegFilter 本机 ffmpeg 没有某个滤镜时跳过（例如 Homebrew 默认不带 drawtext，需要 libfreetype）。
// 生产镜像用 Debian 的 ffmpeg，带该滤镜；这里只是让本地测试不因环境差异误报失败。
func requireFFmpegFilter(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").CombinedOutput()
	if err != nil || !strings.Contains(string(out), " "+name+" ") {
		t.Skipf("本机 ffmpeg 不带 %s 滤镜，跳过（生产镜像带有）", name)
	}
}

// fileServer 把目录当作源站，供服务下载。
func fileServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv
}

// newTestServer 默认禁止内网地址（与线上一致）；需要 httptest 源站时由调用方开 AllowPrivateHosts。
func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.Token = testToken
	if cfg.InstanceID == "" {
		cfg.InstanceID = "tst001"
	}
	if cfg.IdentitySecret == "" {
		cfg.IdentitySecret = testSecret
	}
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
	if cfg.Idempotency.Store == "" {
		cfg.Idempotency.Store = IdemStoreMemory
	}
	if cfg.ObjectDir == "" {
		cfg.ObjectDir = filepath.Join(t.TempDir(), "objects")
	}
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
	return postWithKey(t, base, path, body, token, "test-key")
}

// postWithKey 带 Idempotency-Key 发 JSON（key 为空则不带头）。
func postWithKey(t *testing.T, base, path string, body any, token, key string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
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

// apiErrorBody 契约错误体：{"error": {...}}。
type apiErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Class     string `json:"class"`
		Retryable bool   `json:"retryable"`
		Check     string `json:"check"`
		Message   string `json:"message"`
	} `json:"error"`
}

func assertAPIError(t *testing.T, resp *http.Response, status int, code string) apiErrorBody {
	t.Helper()
	var body apiErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != status || body.Error.Code != code {
		t.Fatalf("status=%d code=%q, want %d/%s (body %+v)", resp.StatusCode, body.Error.Code, status, code, body)
	}
	if body.Error.Message == "" || body.Error.Check == "" || body.Error.Class == "" {
		t.Fatalf("错误体缺字段: %+v", body)
	}
	return body
}

func TestNewServerRefusesEmptyToken(t *testing.T) {
	if _, err := NewServer(Config{ObjectDir: t.TempDir()}); err == nil {
		t.Fatal("没有 Token 必须拒绝启动")
	}
}

func TestRejectsMissingOrWrongToken(t *testing.T) {
	ts := newTestServer(t, Config{})
	body := map[string]string{"url": "http://x/a.mp4"}
	assertAPIError(t, post(t, ts.URL, "/v1/mediasvc/probe", map[string]any{"source": body}, ""), http.StatusUnauthorized, "unauthorized")
	assertAPIError(t, post(t, ts.URL, "/v1/mediasvc/probe", map[string]any{"source": body}, "wrong"), http.StatusUnauthorized, "unauthorized")
}

// 旧路径全部移除（契约只保留 /v1/mediasvc/...），只剩 /healthz。
func TestOldV1RoutesAreGone(t *testing.T) {
	ts := newTestServer(t, Config{})
	for _, p := range []string{"/v1/cover", "/v1/probe", "/v1/video/trim", "/v1/compose", "/v1/watermark", "/v1/capabilities", "/v1/validate"} {
		resp := post(t, ts.URL, p, map[string]any{}, testToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 应已移除，status=%d", p, resp.StatusCode)
		}
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 应保留: %v %v", err, resp)
	}
	resp.Body.Close()
}

func TestCapabilitiesReportsWatermarkReadiness(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mediasvc/capabilities", nil)
	resp := send(t, req, testToken)
	var caps map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&caps)
	if resp.StatusCode != http.StatusOK || caps["watermark"] || !caps["ingest"] {
		t.Fatalf("status=%d caps=%v（未配 logo 时 watermark 应为 false）", resp.StatusCode, caps)
	}
}

// makeClip 用 ffmpeg 生成测试视频（带音轨与否由 withAudio 决定，seconds 为时长字符串）。
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
