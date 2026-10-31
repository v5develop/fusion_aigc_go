package javatool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCancelClassification(t *testing.T) {
	var gotMethod, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotAuth = r.Method, r.Header.Get("Authorization")
		if gotAuth != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"code":2001,"msg":"未登录或登录已过期"}`)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/creation/"), "/cancel")
		switch id {
		case "OK":
			_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":true}`)
		case "DONE":
			_, _ = io.WriteString(w, `{"code":3001,"msg":"任务已完成或失败，无法取消"}`)
		case "GONE":
			_, _ = io.WriteString(w, `{"code":4001,"msg":"创作任务不存在: GONE"}`)
		case "EXPIRED":
			_, _ = io.WriteString(w, `{"code":2004,"msg":"刷新令牌无效或已过期"}`) // HTTP 200 + 登录码也算失效
		case "BOOM":
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"code":5001,"msg":"系统内部错误"}`)
		case "HTML":
			w.WriteHeader(502)
			_, _ = io.WriteString(w, `<html>bad gateway</html>`)
		case "SLOW":
			_, _ = io.WriteString(w, `{"code":1004,"msg":"请求过于频繁"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, 0)
	ctx := context.Background()
	cases := []struct {
		bearer, id string
		want       CancelOutcome
	}{
		{"tok", "OK", CancelCancelled},
		{"Bearer tok", "DONE", CancelAlreadyFinished},
		{"tok", "GONE", CancelNotFound},
		{"bad", "OK", CancelUnauthorized},
		{"tok", "EXPIRED", CancelUnauthorized},
		{"tok", "BOOM", CancelTransient},
		{"tok", "HTML", CancelTransient},
		{"tok", "SLOW", CancelTransient},
		{"", "OK", CancelUnauthorized},
	}
	for _, tc := range cases {
		got, err := c.Cancel(ctx, tc.bearer, tc.id)
		if got != tc.want {
			t.Errorf("%s/%s: got %s want %s (%v)", tc.bearer, tc.id, got, tc.want, err)
		}
		if (got == CancelCancelled) != (err == nil) {
			t.Errorf("%s: 只有 cancelled 时 err 为空: %v", tc.id, err)
		}
		if err != nil && strings.Contains(err.Error(), "tok") {
			t.Errorf("错误信息不应带令牌: %v", err)
		}
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("取消应为 POST: %s", gotMethod)
	}
	// 网络错误
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	if got, _ := New(dead.URL, 0).Cancel(ctx, "tok", "OK"); got != CancelTransient {
		t.Fatalf("网络错误应为暂时性: %s", got)
	}
	if got, err := New("", 0).Cancel(ctx, "tok", "OK"); got != CancelTransient || !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("未配置: %s %v", got, err)
	}
}
