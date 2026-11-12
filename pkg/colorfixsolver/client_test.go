package colorfixsolver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, c Conf) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c.Endpoint = srv.URL + "/" // 末尾斜杠要被去掉
	cl := New(c)
	if cl == nil {
		t.Fatal("client nil")
	}
	return cl
}

func TestDisabledWhenNoEndpoint(t *testing.T) {
	if cl := New(Conf{}); cl != nil || cl.Enabled() {
		t.Fatalf("空 Endpoint 应关闭: %v", cl)
	}
	var nilc *Client
	if _, err := nilc.Solve(context.Background(), Request{}); KindOf(err) != KindUnavailable {
		t.Fatalf("nil 客户端 err=%v", err)
	}
}

func TestSolveRequestAndResponse(t *testing.T) {
	var got struct {
		auth, ctype, path, method string
		body                      Request
	}
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.auth, got.ctype, got.path, got.method = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = w.Write([]byte(`{"pieces":[{"key":"top","params":{"hue":-9,"saturation":2.4,"value":0,"brightness":-7,"contrast":3},"score":97.1}],"version":"colorfix-solver/0.1.0","extra":1}`))
	}, Conf{Token: "tok-123"})
	resp, err := cl.Solve(context.Background(), Request{ImageURL: "https://x/r.png",
		Reference: []Ref{{Key: "top", URL: "https://x/g.png"}}, Masks: []Mask{{Key: "top", URL: "https://x/m.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/v1/solve" || got.auth != "Bearer tok-123" || got.ctype != "application/json" {
		t.Fatalf("请求头 / 路径不对: %+v", got)
	}
	if got.body.ImageURL != "https://x/r.png" || got.body.Reference[0].Key != "top" || got.body.Masks[0].URL != "https://x/m.png" {
		t.Fatalf("请求体 %+v", got.body)
	}
	if len(resp.Pieces) != 1 || resp.Pieces[0].Params.Saturation != 2.4 || resp.Pieces[0].Score != 97.1 || resp.Version != "colorfix-solver/0.1.0" {
		t.Fatalf("响应 %+v", resp)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   Kind
		code   string
	}{
		{429, KindBusy, "busy"},
		{504, KindTimeout, "timeout"},
		{422, KindUnusable, "bad_mask"},
		{401, KindUnavailable, "unauthorized"},
		{403, KindUnavailable, "forbidden_fetch"},
		{400, KindUnavailable, "invalid"},
		{500, KindUnavailable, "internal"},
		{502, KindUnavailable, "fetch_failed"},
	}
	for _, c := range cases {
		cl := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"code":"` + c.code + `","message":"m"}}`))
		}, Conf{})
		_, err := cl.Solve(context.Background(), Request{})
		e, ok := err.(*Error)
		if !ok || e.Kind != c.want || e.Status != c.status || e.Code != c.code || e.Message != "m" {
			t.Errorf("status=%d err=%v", c.status, err)
		}
	}
}

func TestNonJSONSuccessBody(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, Conf{})
	if _, err := cl.Solve(context.Background(), Request{}); KindOf(err) != KindUnavailable {
		t.Fatalf("err=%v", err)
	}
}

func TestClientTimeoutIsTimeoutKind(t *testing.T) {
	release := make(chan struct{})
	cl := newTestClient(t, func(http.ResponseWriter, *http.Request) { <-release }, Conf{})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := cl.Solve(ctx, Request{}); KindOf(err) != KindTimeout {
		t.Fatalf("err=%v", err)
	}
}

func TestConnectRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	cl := New(Conf{Endpoint: url})
	if _, err := cl.Solve(context.Background(), Request{}); KindOf(err) != KindUnavailable {
		t.Fatalf("err=%v", err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(other.Close)
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }, Conf{Token: "secret-token-000"})
	if _, err := cl.Solve(context.Background(), Request{}); KindOf(err) != KindUnavailable || hit {
		t.Fatalf("重定向不应被跟随（会带走 token）: err=%v hit=%v", err, hit)
	}
}

// 并发满：立刻按忙返回，不排队；放行一个后恢复。
func TestLocalConcurrencyLimit(t *testing.T) {
	enter, release := make(chan struct{}, 4), make(chan struct{})
	cl := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		enter <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"pieces":[],"version":"v"}`))
	}, Conf{MaxConcurrent: 1})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := cl.Solve(context.Background(), Request{}); err != nil {
			t.Errorf("第一个请求: %v", err)
		}
	}()
	<-enter
	if _, err := cl.Solve(context.Background(), Request{}); KindOf(err) != KindBusy {
		t.Fatalf("并发满应为 busy: %v", err)
	}
	close(release)
	wg.Wait()
	if _, err := cl.Solve(context.Background(), Request{}); KindOf(err) == KindBusy {
		t.Fatalf("名额应已释放: %v", err)
	}
}
