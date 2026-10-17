package idempotency

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolvePrefersIdempotencyKey(t *testing.T) {
	cases := []struct {
		name, idem, requestID, want string
	}{
		{"只带 Idempotency-Key", "k-1", "", "k-1"},
		{"只带 X-Request-Id（旧版本）", "", "r-1", "r-1"},
		{"两个都带且值相同", "k-1", "k-1", "k-1"},
		{"两个都带且值不同：优先 Idempotency-Key，不报错", "k-1", "trace-9", "k-1"},
		{"两个都没带", "", "", ""},
		{"首尾空白忽略", " k-1 ", "", "k-1"},
	}
	for _, c := range cases {
		if got := Resolve(c.idem, c.requestID); got != c.want {
			t.Errorf("%s: 应为 %q，实际 %q", c.name, c.want, got)
		}
	}
}

// 旧版本只带 X-Request-Id：补成 Idempotency-Key，X-Request-Id 保持不变
func TestMiddlewareFillsKeyFromLegacyHeader(t *testing.T) {
	var gotKey, gotRequestID string
	h := Middleware(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(HeaderIdempotencyKey)
		gotRequestID = r.Header.Get(HeaderRequestID)
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(HeaderRequestID, "r-1")
	h(httptest.NewRecorder(), req)
	if gotKey != "r-1" {
		t.Fatalf("Idempotency-Key 应补成 r-1，实际 %q", gotKey)
	}
	if gotRequestID != "r-1" {
		t.Fatalf("X-Request-Id 不应被改写，实际 %q", gotRequestID)
	}
}

// 两个都带且值不同：Idempotency-Key 不变（幂等用它），X-Request-Id 原样保留（链路追踪用它）
func TestMiddlewareKeepsTraceIDWhenBothSent(t *testing.T) {
	var gotKey, gotRequestID string
	h := Middleware(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(HeaderIdempotencyKey)
		gotRequestID = r.Header.Get(HeaderRequestID)
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(HeaderIdempotencyKey, "k-1")
	req.Header.Set(HeaderRequestID, "trace-9")
	h(httptest.NewRecorder(), req)
	if gotKey != "k-1" {
		t.Fatalf("幂等键应为 k-1，实际 %q", gotKey)
	}
	if gotRequestID != "trace-9" {
		t.Fatalf("链路追踪 ID 应保持 trace-9，实际 %q", gotRequestID)
	}
}

// 两个都没带：什么都不写，交给接口的必填校验
func TestMiddlewareLeavesEmptyAlone(t *testing.T) {
	var gotKey string
	h := Middleware(func(w http.ResponseWriter, r *http.Request) { gotKey = r.Header.Get(HeaderIdempotencyKey) })
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if gotKey != "" {
		t.Fatalf("无幂等键时不应写入，实际 %q", gotKey)
	}
}
