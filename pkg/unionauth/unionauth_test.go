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

// 与 Java UnionTokenAuthService 对齐：HTTP 正常但业务码不成功 → 拒绝（PARAM_INVALID）；
// HTTP 4xx/5xx、解析失败、没有 data → 联盟不可用（THIRD_PARTY_ERROR）。
func TestLoginClassifiesErrors(t *testing.T) {
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		rejected bool   // true: ErrRejected；false: ErrUnavailable
		msg      string // rejected 时联盟给的文案
	}{
		{"业务码非 1 取联盟 message", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":401,"message":"token 已过期"}`))
		}, true, "token 已过期"},
		{"业务码非 1 没有 message 用默认文案", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":401}`))
		}, true, "Token无效或已过期"},
		{"用户信息缺 userId", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":1,"data":{"nickname":"x"}}`))
		}, true, "联盟用户ID无效"},
		{"HTTP 401 按不可用处理", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, false, ""},
		{"HTTP 500 按不可用处理", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, false, ""},
		{"响应不是 JSON", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<html>bad gateway</html>`))
		}, false, ""},
		{"成功码但没有 data", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":1,"message":"ok"}`))
		}, false, ""},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.handler)
		_, err := New(Conf{BaseURL: srv.URL}).Login(context.Background(), "x")
		srv.Close()
		if c.rejected {
			var rej *RejectedError
			if !errors.Is(err, ErrRejected) || !errors.As(err, &rej) {
				t.Errorf("%s: 应判为联盟拒绝，实际 %v", c.name, err)
				continue
			}
			if rej.Message != c.msg {
				t.Errorf("%s: 文案应为 %q，实际 %q", c.name, c.msg, rej.Message)
			}
			if errors.Is(err, ErrUnavailable) {
				t.Errorf("%s: 不应同时判为不可用", c.name)
			}
		} else {
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("%s: 应判为联盟不可用，实际 %v", c.name, err)
			}
			if errors.Is(err, ErrRejected) {
				t.Errorf("%s: 不应判为联盟拒绝", c.name)
			}
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
