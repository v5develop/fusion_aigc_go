package media

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const secretHeaderValue = "SECRET-VALUE-7f3a9c"

// authOrigin 源站：只有带正确 Authorization 与 UA 才给文件，否则 403。
func authOrigin(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	fs := http.FileServer(http.Dir(dir))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretHeaderValue || r.Header.Get("User-Agent") != "fusion-test/1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ingestWithHeaders(t *testing.T, base, url string, headers map[string]string, key string) *http.Response {
	t.Helper()
	return postWithKey(t, base, "/v1/mediasvc/ingest", map[string]any{
		"source":  map[string]any{"url": url, "headers": headers},
		"purpose": "third_party",
	}, testToken, key)
}

// 白名单内的头随请求发给源站：源站校验通过，入库成功。
func TestSourceHeadersReachOrigin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.ttf"), fontFile("\x00\x01\x00\x00", 512), 0o600); err != nil {
		t.Fatal(err)
	}
	origin := authOrigin(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	res := decodeMediaResult(t, ingestWithHeaders(t, ts.URL, origin.URL+"/f.ttf",
		map[string]string{"Authorization": "Bearer " + secretHeaderValue, "user-agent": "fusion-test/1"}, "k-hdr-ok"))
	if res.Object.Mime != "font/ttf" {
		t.Fatalf("mime = %s", res.Object.Mime)
	}
}

// 白名单之外的头一律 400（不静默丢弃）；头只允许在 ingest 的 url 来源上使用。
func TestSourceHeadersRejectedOutsideWhitelistAndScope(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	cases := []struct {
		name string
		resp *http.Response
	}{
		{"cookie not allowed", ingestWithHeaders(t, ts.URL, "http://127.0.0.1:1/a", map[string]string{"Cookie": "a=b"}, "k-h1")},
		{"host not allowed", ingestWithHeaders(t, ts.URL, "http://127.0.0.1:1/a", map[string]string{"Host": "evil"}, "k-h2")},
		{"x-header not allowed", ingestWithHeaders(t, ts.URL, "http://127.0.0.1:1/a", map[string]string{"X-Token": "1"}, "k-h3")},
		{"duplicate after canonicalization", ingestWithHeaders(t, ts.URL, "http://127.0.0.1:1/a",
			map[string]string{"accept": "a", "Accept": "b"}, "k-h4")},
		{"crlf in value", ingestWithHeaders(t, ts.URL, "http://127.0.0.1:1/a",
			map[string]string{"Accept": "text/plain\r\nX-Inject: 1"}, "k-h5")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertAPIError(t, c.resp, http.StatusBadRequest, "invalid_request")
		})
	}
	// rid 来源不能带头
	resp := postWithKey(t, ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source":  map[string]any{"rid": "tst001-user-aaaaaaaaaaaaaaaaaaaaaaaaaa", "headers": map[string]string{"Accept": "*/*"}},
		"purpose": "user_input",
	}, testToken, "k-h6")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
	// 其它写操作的 url 来源不接受头
	resp = postWithKey(t, ts.URL, "/v1/mediasvc/probe", map[string]any{
		"source": map[string]any{"url": "http://127.0.0.1:1/a", "headers": map[string]string{"Accept": "*/*"}},
	}, testToken, "")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

// 跳到别的来源（不同端口即不同主机）时，调用方的头被剥掉；同来源跳转则保留。
func TestSourceHeadersStrippedOnCrossHostRedirect(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.ttf"), fontFile("\x00\x01\x00\x00", 512), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var otherAuth, otherUA string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherAuth, otherUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		mu.Unlock()
		http.ServeFile(w, r, filepath.Join(dir, "f.ttf"))
	}))
	t.Cleanup(other.Close)

	fs := http.FileServer(http.Dir(dir))
	var selfAuth string
	var mainSrv *httptest.Server
	mainSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-other":
			http.Redirect(w, r, other.URL+"/f.ttf", http.StatusFound)
		case "/to-self":
			http.Redirect(w, r, mainSrv.URL+"/f.ttf", http.StatusFound)
		case "/f.ttf":
			selfAuth = r.Header.Get("Authorization")
			if r.Header.Get("Authorization") != "Bearer "+secretHeaderValue {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			fs.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mainSrv.Close)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	headers := map[string]string{"Authorization": "Bearer " + secretHeaderValue, "User-Agent": "fusion-test/1"}

	// 跨主机：目标站收不到任何调用方的头
	decodeMediaResult(t, ingestWithHeaders(t, ts.URL, mainSrv.URL+"/to-other", headers, "k-redir-other"))
	mu.Lock()
	// User-Agent 为空或是 Go 默认值都可以，但绝不能是调用方给的值
	if otherAuth != "" || otherUA == "fusion-test/1" {
		t.Fatalf("跨主机跳转泄露了头: Authorization=%q User-Agent=%q", otherAuth, otherUA)
	}
	mu.Unlock()

	// 同主机：头保留，源站仍能鉴权通过
	decodeMediaResult(t, ingestWithHeaders(t, ts.URL, mainSrv.URL+"/to-self", headers, "k-redir-self"))
	if selfAuth != "Bearer "+secretHeaderValue {
		t.Fatalf("同主机跳转应保留 Authorization，实际 %q", selfAuth)
	}
}

// 头的值不出现在响应体与日志里（成功、422、503 三种结局都查）。
func TestSourceHeaderValuesNeverLogged(t *testing.T) {
	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(old) })

	dir := t.TempDir()
	forbidden := authOrigin(t, dir) // 没有文件：403 → 422
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError) // 5xx → 503
	}))
	t.Cleanup(failing.Close)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	headers := map[string]string{"Authorization": "Bearer " + secretHeaderValue, "User-Agent": "fusion-test/1"}

	for i, url := range []string{forbidden.URL + "/missing.ttf", failing.URL + "/x.ttf"} {
		resp := ingestWithHeaders(t, ts.URL, url, headers, "k-leak-"+string(rune('a'+i)))
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		if strings.Contains(buf.String(), secretHeaderValue) {
			t.Fatalf("响应体含头的值: %s", buf.String())
		}
	}
	if strings.Contains(logBuf.String(), secretHeaderValue) {
		t.Fatalf("日志含头的值: %s", logBuf.String())
	}
}

// 缓存隔离：带头下载的内容不会被不带头的请求命中（否则等于绕过鉴权）。
func TestSourceHeaderCacheIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.ttf"), fontFile("\x00\x01\x00\x00", 512), 0o600); err != nil {
		t.Fatal(err)
	}
	origin := authOrigin(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	headers := map[string]string{"Authorization": "Bearer " + secretHeaderValue, "User-Agent": "fusion-test/1"}
	decodeMediaResult(t, ingestWithHeaders(t, ts.URL, origin.URL+"/f.ttf", headers, "k-cache-auth"))

	// 不带头的请求：源站返回 403，按瞬时处理（503），绝不能命中缓存而成功
	resp := postWithKey(t, ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"url": origin.URL + "/f.ttf"}, "purpose": "third_party",
	}, testToken, "k-cache-noauth")
	assertAPIError(t, resp, http.StatusServiceUnavailable, "transient")
}

// 头不参与 rid 派生：同一个幂等键，带头的 url 来源与字节上传得到同一个 rid。
func TestSourceHeadersDoNotChangeRID(t *testing.T) {
	dir := t.TempDir()
	font := fontFile("\x00\x01\x00\x00", 512)
	if err := os.WriteFile(filepath.Join(dir, "f.ttf"), font, 0o600); err != nil {
		t.Fatal(err)
	}
	origin := authOrigin(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	headers := map[string]string{"Authorization": "Bearer " + secretHeaderValue, "User-Agent": "fusion-test/1"}
	viaURL := decodeMediaResult(t, ingestWithHeaders(t, ts.URL, origin.URL+"/f.ttf", headers, "k-rid-same"))
	viaBytes := decodeMediaResult(t, postRaw(t, ts.URL+"/v1/mediasvc/ingest/upload?purpose=third_party", font, "k-rid-same"))
	if viaURL.Object.RID != viaBytes.Object.RID {
		t.Fatalf("rid 不一致: %s vs %s", viaURL.Object.RID, viaBytes.Object.RID)
	}
}

// 派生的缓存键只含 URL 与头的摘要，不含头的值；值变了摘要就变。
func TestHeaderDigestHidesValues(t *testing.T) {
	h1, err := validateHeaders(map[string]string{"Authorization": "Bearer AAA"})
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := validateHeaders(map[string]string{"Authorization": "Bearer BBB"})
	k1, k2 := cacheKeyFor("https://x/a", h1), cacheKeyFor("https://x/a", h2)
	if strings.Contains(k1, "AAA") || k1 == k2 || cacheKeyFor("https://x/a", nil) != "https://x/a" {
		t.Fatalf("keys = %q %q", k1, k2)
	}
}

// 名单为空（默认）：带调用方凭证的 ingest 一律拒绝，且不向源站发出任何请求。
func TestSourceHeadersRefusedWhenHostNotAllowlisted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.ttf"), fontFile("\x00\x01\x00\x00", 512), 0o600); err != nil {
		t.Fatal(err)
	}
	origin := authOrigin(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true})
	resp := ingestWithHeaders(t, ts.URL, origin.URL+"/f.ttf",
		map[string]string{"Authorization": "Bearer " + secretHeaderValue}, "k-hdr-deny")
	assertAPIError(t, resp, http.StatusForbidden, "forbidden_purpose")
}

// 带请求头的下载不进共享缓存：同一个 URL 连续两次都真正访问源站。
func TestHeaderedDownloadsBypassTheSharedCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.ttf"), fontFile("\x00\x01\x00\x00", 512), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := http.FileServer(http.Dir(dir))
	var hits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secretHeaderValue {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, IngestHeaderHosts: "127.0.0.1"})
	hdr := map[string]string{"Authorization": "Bearer " + secretHeaderValue}
	_ = decodeMediaResult(t, ingestWithHeaders(t, ts.URL, origin.URL+"/f.ttf", hdr, "k-nocache-1"))
	_ = decodeMediaResult(t, ingestWithHeaders(t, ts.URL, origin.URL+"/f.ttf", hdr, "k-nocache-2"))
	if n := hits.Load(); n != 2 {
		t.Fatalf("带凭证的下载不应命中缓存：源站被访问 %d 次，want 2", n)
	}
}
