package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestPublicIP(t *testing.T) {
	blocked := []string{"100.100.100.200", "100.64.0.1", "127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.169.254",
		"0.1.2.3", "198.18.0.5", "240.0.0.1", "::1", "fd00::1", "::ffff:127.0.0.1",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::6464:64c8", // NAT64 内嵌回环 / 元数据 / CGNAT
		"2002:7f00:1::1", "2002:a9fe:a9fe::1", "64:ff9b:1::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2"}
	for _, s := range blocked {
		if PublicIP(net.ParseIP(s)) {
			t.Errorf("%s 不应被当作公网地址", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "47.100.1.1", "2606:4700:4700::1111", "64:ff9b::808:808"} {
		if !PublicIP(net.ParseIP(s)) {
			t.Errorf("%s 应当是公网地址", s)
		}
	}
}

func TestClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := NewClient(5*time.Second, false).Do(req); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("回环地址应被拦，实际 %v", err)
	}
	resp, err := NewClient(5*time.Second, true).Do(req)
	if err != nil {
		t.Fatalf("allowPrivate 应放行: %v", err)
	}
	resp.Body.Close()
}

func TestRedirectCapAndForbidden(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/x", http.StatusFound) // 无限自跳转
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := NewClient(5*time.Second, true).Do(req); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("应因跳转过多失败，实际 %v", err)
	}
	if err := CheckRedirect(false)(&http.Request{URL: mustURL("http://169.254.169.254/")}, []*http.Request{{}}); !errors.Is(err, ErrRedirectForbidden) {
		t.Fatalf("跳到元数据地址应被拒，实际 %v", err)
	}
	if err := CheckRedirect(false)(&http.Request{URL: mustURL("file:///etc/passwd")}, []*http.Request{{}}); !errors.Is(err, ErrRedirectForbidden) {
		t.Fatalf("非 http 协议应被拒，实际 %v", err)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
