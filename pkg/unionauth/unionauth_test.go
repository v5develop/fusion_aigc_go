package unionauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 前端给的临时 token 换用户信息（联盟的成功码是 1）
func TestLogin(t *testing.T) {
	var gotTempPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			gotTempPath.Store(r.URL.Path)
			_, _ = w.Write([]byte(`{"code":1,"message":"ok","data":{"userId":206757090755100672,"nickname":"小林","phone":"130****7881","status":"active"}}`))
		default:
			t.Errorf("没想到的请求: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	u, err := New(Conf{BaseURL: srv.URL}).Login(context.Background(), "tmp-123")
	if err != nil {
		t.Fatal(err)
	}
	if u.UserID.String() != "206757090755100672" || u.Nickname != "小林" {
		t.Fatalf("用户信息不对: %+v", u)
	}
	if gotTempPath.Load() != "/api/v1/auth/tmp-123/user" {
		t.Errorf("临时 token 应该在路径里: %v", gotTempPath.Load())
	}
}

func TestLoginUnauthorized(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"业务码非 1": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":401,"message":"token 已过期"}`))
		},
		"HTTP 401": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		_, err := New(Conf{BaseURL: srv.URL}).Login(context.Background(), "x")
		srv.Close()
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: 应该判为登录失效，实际 %v", name, err)
		}
	}
}

func TestEnabled(t *testing.T) {
	if New(Conf{}).Enabled() {
		t.Error("没配地址应该是未开放")
	}
	if !New(Conf{BaseURL: "http://x"}).Enabled() {
		t.Error("配了地址应该可用")
	}
}
