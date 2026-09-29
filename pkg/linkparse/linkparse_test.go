package linkparse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 抖音：裸请求能下、不需要 UA，标题就是文案
func TestParseDouyin(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/parse" || r.Method != http.MethodPost {
			t.Errorf("路径或方法不对: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"platform":"douyin","source_url":"https://www.douyin.com/video/123",
			"title":"西湖边这家咖啡店","cover":{"url":"https://cdn/cover.jpg","kind":"cover"},
			"medias":[{"url":"https://cdn/v.mp4","kind":"video","duration":15.2,"user_agent":null}],
			"needs_mux":false}`))
	}))
	defer srv.Close()

	v, err := New(Conf{BaseURL: srv.URL}).Parse(context.Background(), "7.99 复制打开抖音 https://v.douyin.com/abc/ 看看")
	if err != nil {
		t.Fatal(err)
	}
	if v.URL != "https://cdn/v.mp4" || v.Title != "西湖边这家咖啡店" || v.CoverURL != "https://cdn/cover.jpg" {
		t.Fatalf("解析结果不对: %+v", v)
	}
	if v.Duration != 15.2 || v.SourceURL != "https://www.douyin.com/video/123" || v.UserAgent != "" {
		t.Fatalf("解析结果不对: %+v", v)
	}
	// 整段文案原样透传，平台交给对方识别
	if got["url"] != "7.99 复制打开抖音 https://v.douyin.com/abc/ 看看" {
		t.Errorf("没把原文案透传: %v", got["url"])
	}
	// B站 走单文件，免得拿到哑片
	if got["prefer_quality"] != false {
		t.Errorf("prefer_quality 应该是 false: %v", got["prefer_quality"])
	}
}

// B站：要带 UA 才能下
func TestParseKeepsUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"platform":"bilibili","source_url":"https://b23.tv/x","title":"t",
			"medias":[{"url":"https://cdn/v.mp4","kind":"video","user_agent":"Mozilla/5.0 test"}],
			"needs_mux":false}`))
	}))
	defer srv.Close()

	v, err := New(Conf{BaseURL: srv.URL}).Parse(context.Background(), "https://b23.tv/x")
	if err != nil {
		t.Fatal(err)
	}
	if v.UserAgent != "Mozilla/5.0 test" {
		t.Fatalf("UA 没带出来: %+v", v)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"平台不支持/没有链接", 422, `{"detail":"unsupported"}`, ErrUnsupported},
		{"引擎全挂", 502, `{"detail":"all engines failed"}`, ErrUpstream},
		{"预热未就绪", 503, `{"detail":"warming up"}`, ErrUpstream},
		{"图集作品没有视频", 200,
			`{"platform":"xhs","source_url":"x","medias":[{"url":"https://cdn/1.jpg","kind":"image"}]}`, ErrNoVideo},
		{"音视频分离会得到哑片", 200,
			`{"platform":"bilibili","source_url":"x","needs_mux":true,
			  "medias":[{"url":"https://cdn/v.mp4","kind":"video"},{"url":"https://cdn/a.m4a","kind":"audio"}]}`,
			ErrNoVideo},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(Conf{BaseURL: srv.URL}).Parse(context.Background(), "x")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: 应该是 %v，实际 %v", tc.name, tc.want, err)
		}
	}
}

func TestEnabled(t *testing.T) {
	if New(Conf{}).Enabled() || New(Conf{BaseURL: "  "}).Enabled() {
		t.Error("没配地址不该启用")
	}
	if !New(Conf{BaseURL: "http://x"}).Enabled() {
		t.Error("配了地址应该启用")
	}
}

// 下载：带 UA、不带 Referer、超上限立刻停
func TestDownload(t *testing.T) {
	big := strings.Repeat("v", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" {
			t.Error("不该带 Referer，带了反而 403")
		}
		if r.Header.Get("User-Agent") != "ua-test" {
			t.Errorf("UA 没带上: %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "video/mp4; charset=binary")
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	v := &Video{URL: srv.URL, UserAgent: "ua-test"}
	data, ct, err := Download(context.Background(), srv.Client(), v, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1000 || ct != "video/mp4" {
		t.Fatalf("下载结果不对: %d 字节, %s", len(data), ct)
	}
	if _, _, err := Download(context.Background(), srv.Client(), v, 500); !errors.Is(err, ErrTooLarge) {
		t.Errorf("超上限应该被拦住，实际 %v", err)
	}
}

// 直链常常不给 Content-Type，兜底成 video/mp4
func TestDownloadDefaultsContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	_, ct, err := Download(context.Background(), srv.Client(), &Video{URL: srv.URL}, 100)
	if err != nil || ct != "video/mp4" {
		t.Fatalf("ct = %q, err = %v", ct, err)
	}
}

// 解析服务「这条链接解析不出来」和「服务挂了」返回的都是 502，
// 只能靠响应体里有没有 attempts 区分。分错了的后果是用户看到"请稍后重试"，
// 然后一直重试——而重试永远不会成功。
func TestParseFailureIsNotUpstreamOutage(t *testing.T) {
	// 实测响应体（探测一条假抖音链接得到的）
	const body = `{"detail":{"error":"全部引擎失败，最后原因: ValueError: Failed to parse video ID from PC share URL",` +
		`"detail":null,"platform":"douyin","attempts":["f2@original","f2@original#2","TikTokDownloader@original",` +
		`"parse-video-py@original","parse-video-py@original#2","parse-video-py@canonical","parse-video-py@canonical#2"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, err := New(Conf{BaseURL: srv.URL}).Parse(context.Background(), "https://v.douyin.com/x/")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("跑过引擎的 502 应判为链接不支持（重试没用），实际 %v", err)
	}
	if errors.Is(err, ErrUpstream) {
		t.Fatalf("不该判成服务不可用：%v", err)
	}
}

// 真的挂了（502 但不是它的业务响应）还是要判成可重试。
func TestRealOutageStaysUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><head><title>502 Bad Gateway</title></head></html>"))
	}))
	defer srv.Close()

	_, err := New(Conf{BaseURL: srv.URL}).Parse(context.Background(), "https://v.douyin.com/x/")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("nginx 的 502 应判为服务暂时不可用，实际 %v", err)
	}
}
