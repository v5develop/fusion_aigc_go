package media

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// ---- Retry-After 透传 ----

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"soon", 0},
		{"-3", 0},
		{"0", 1},
		{"7", 7},
		{" 42 ", 42},
		{"120", 120},
		{"121", 120},
		{"86400", 120},
		{"99999999999999999999999", 120},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30},            // IMF-fixdate
		{now.Add(1500 * time.Millisecond).Format(http.TimeFormat), 1},      // 秒级精度：格式化后截掉毫秒
		{now.Add(45 * time.Second).Format(time.RFC850), 45},                // RFC 850
		{now.Add(10 * time.Second).Format(time.ANSIC), 10},                 // asctime
		{now.Add(-time.Minute).Format(http.TimeFormat), 1},                 // 已过去：按 1 秒
		{now.Add(time.Hour).Format(http.TimeFormat), maxRetryAfterSeconds}, // 超上限截断
		{now.Add(30 * time.Second).Format(time.RFC3339), 0},                // 非 HTTP 日期格式：忽略
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in, now); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// probeRetryAfter 源站以 status + Retry-After 拒绝，经完整 HTTP 入口（probe）返回给调用方的状态与 Retry-After。
func probeRetryAfter(t *testing.T, status int, retryAfter string) (int, string, apiErrorBody) {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(origin.Close)
	ts := newTestServer(t, Config{AllowPrivateHosts: true})
	resp := post(t, ts.URL, "/v1/mediasvc/probe", map[string]any{"source": map[string]string{"url": origin.URL + "/a.mp4"}}, testToken)
	ra := resp.Header.Get("Retry-After")
	body := assertAPIError(t, resp, http.StatusServiceUnavailable, "transient")
	return resp.StatusCode, ra, body
}

func TestUpstream429RetryAfterSecondsPassedThrough(t *testing.T) {
	_, ra, body := probeRetryAfter(t, http.StatusTooManyRequests, "17")
	if ra != "17" || !body.Error.Retryable {
		t.Fatalf("源站 Retry-After: 17 应原样透传，得到 %q %+v", ra, body.Error)
	}
}

func TestUpstream429RetryAfterHTTPDatePassedThrough(t *testing.T) {
	// 日期按秒取整，测试时钟与服务端时钟之间有毫秒级误差：接受 [29, 30]
	_, ra, _ := probeRetryAfter(t, http.StatusTooManyRequests, time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	n, err := strconv.Atoi(ra)
	if err != nil || n < 29 || n > 30 {
		t.Fatalf("HTTP 日期格式的 Retry-After 应换算成约 30 秒，得到 %q", ra)
	}
}

func TestUpstream503RetryAfterPassedThroughAndCapped(t *testing.T) {
	if _, ra, _ := probeRetryAfter(t, http.StatusServiceUnavailable, "3600"); ra != strconv.Itoa(maxRetryAfterSeconds) {
		t.Fatalf("超过上限应截到 %d，得到 %q", maxRetryAfterSeconds, ra)
	}
}

func TestUpstream429WithoutRetryAfterDefaultsToFive(t *testing.T) {
	if _, ra, _ := probeRetryAfter(t, http.StatusTooManyRequests, ""); ra != "5" {
		t.Fatalf("源站没给 Retry-After 时应保持 5 秒，得到 %q", ra)
	}
	if _, ra, _ := probeRetryAfter(t, http.StatusTooManyRequests, "garbage"); ra != "5" {
		t.Fatalf("非法 Retry-After 应回落 5 秒，得到 %q", ra)
	}
}

// ---- 重定向错误分类 ----

// 重定向次数过多：素材地址本身的问题，422 source_unavailable，不可重试。
func TestRedirectLoopIsPermanentSourceUnavailable(t *testing.T) {
	var loop *httptest.Server
	loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loop.URL+r.URL.Path+"x", http.StatusFound)
	}))
	t.Cleanup(loop.Close)
	ae := downloadClass(t, loop.URL+"/a")
	if ae.Code != codeSourceUnavailable || ae.status != http.StatusUnprocessableEntity || ae.Retryable ||
		ae.Check != "source.redirect_too_many" {
		t.Fatalf("重定向次数过多应为 422 source_unavailable: code=%s status=%d check=%s retryable=%v", ae.Code, ae.status, ae.Check, ae.Retryable)
	}
}

// 重定向到非 http/https 协议：422 source_unavailable。
func TestRedirectToForbiddenSchemeIsPermanent(t *testing.T) {
	for _, target := range []string{"file:///etc/passwd", "ftp://example.com/a.mp4", "gopher://127.0.0.1:6379/_x"} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(origin.Close)
		ae := downloadClass(t, origin.URL+"/a.mp4")
		if ae.Code != codeSourceUnavailable || ae.status != http.StatusUnprocessableEntity || ae.Check != "source.redirect_forbidden" {
			t.Errorf("重定向到 %s 应为 422 source_unavailable redirect_forbidden: code=%s status=%d check=%s", target, ae.Code, ae.status, ae.Check)
		}
	}
}

// 重定向到内网 / 回环 / 元数据地址（IP 字面量）：CheckRedirect 当场拒绝，422 source_unavailable。
func TestCheckRedirectRejectsPrivateIPTarget(t *testing.T) {
	f := newFetcher(Config{FetchTimeoutSeconds: 1, MaxInputBytes: 1 << 20}) // 不开私网
	first, _ := http.NewRequest(http.MethodGet, "https://cdn.example.com/a.mp4", nil)
	for _, target := range []string{"http://127.0.0.1/a", "http://10.0.0.8/a", "http://100.100.100.200/latest/meta-data", "http://[::1]/a"} {
		u, _ := url.Parse(target)
		next := &http.Request{URL: u, Header: http.Header{}}
		next = next.WithContext(context.Background())
		err := f.client.CheckRedirect(next, []*http.Request{first})
		if err == nil {
			t.Errorf("跳转到 %s 应被拒绝", target)
			continue
		}
		ae := classifyError(classifyFetchError(&url.Error{Op: "Get", URL: target, Err: err}, true))
		if ae.Code != codeSourceUnavailable || ae.Check != "source.redirect_forbidden" || ae.Retryable {
			t.Errorf("跳转到 %s: code=%s check=%s", target, ae.Code, ae.Check)
		}
	}
	// 公网地址照常放行
	u, _ := url.Parse("https://8.8.8.8/a")
	if err := f.client.CheckRedirect((&http.Request{URL: u, Header: http.Header{}}).WithContext(context.Background()), []*http.Request{first}); err != nil {
		t.Fatalf("跳转到公网地址不应被拒: %v", err)
	}
}

// 域名在建连时才解析到内网（Dial 的 Control 拦下）：发生在跳转之后归为重定向到被禁止的地址（422 source_unavailable），
// 首跳就是内网保持原分类（422 source_rejected），两者都不是瞬时。
func TestBlockedAddressClassificationByRedirect(t *testing.T) {
	dialErr := &url.Error{Op: "Get", URL: "http://internal.example/a", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errBlockedAddress}}
	if ae := classifyError(classifyFetchError(dialErr, true)); ae.Code != codeSourceUnavailable || ae.Check != "source.redirect_forbidden" {
		t.Fatalf("跳转后被拦: code=%s check=%s", ae.Code, ae.Check)
	}
	if ae := classifyError(classifyFetchError(dialErr, false)); ae.Code != codeSourceRejected || ae.status != http.StatusUnprocessableEntity {
		t.Fatalf("首跳被拦应保持 source_rejected: code=%s", ae.Code)
	}
}

// 端到端：严格模式（不开私网）下，经由跳转才到达内网地址。首跳用一个只在测试里放行的拨号器连到本地源站，
// 跳转目标是回环地址的域名形式（localhost），由 Dial 的 Control 拦下。
func TestRedirectToPrivateHostnameEndToEnd(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	t.Cleanup(target.Close)
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+port+"/a.mp4", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	f := newFetcher(Config{FetchTimeoutSeconds: 2, MaxInputBytes: 1 << 20})
	strictDial := f.client.Transport.(*http.Transport).DialContext
	originAddr := origin.Listener.Addr().String()
	// 只有首跳（源站）绕过 SSRF 检查，模拟"公网源站"；其余连接走原来的严格拨号器
	f.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == originAddr {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		return strictDial(ctx, network, addr)
	}
	_, err := f.download(context.Background(), origin.URL+"/a.mp4", t.TempDir(), "x.bin")
	ae := classifyError(err)
	if ae.Code != codeSourceUnavailable || ae.status != http.StatusUnprocessableEntity || ae.Check != "source.redirect_forbidden" {
		t.Fatalf("跳转到内网域名应为 422 source_unavailable: code=%s status=%d check=%s err=%v", ae.Code, ae.status, ae.Check, err)
	}
}

// 跳转后的网络错误（连接被拒）仍是瞬时：503 transient。
func TestNetworkErrorAfterRedirectStaysTransient(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://%s/a.mp4", closed), http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	ae := downloadClass(t, origin.URL+"/a.mp4")
	if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
		t.Fatalf("跳转后的连接失败应为 transient: code=%s status=%d", ae.Code, ae.status)
	}
}
