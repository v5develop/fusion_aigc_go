package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadyOKAndFail(t *testing.T) {
	var n atomic.Int32
	r := NewReady(Options{TTL: time.Hour}, Check{"a", func(context.Context) error { n.Add(1); return nil }})
	rec := httptest.NewRecorder()
	r.Handler()(rec, httptest.NewRequest("GET", "/readyz", nil))
	r.Handler()(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 || n.Load() != 1 {
		t.Fatalf("code=%d 检查次数=%d（缓存应只查一次）", rec.Code, n.Load())
	}

	bad := NewReady(Options{}, Check{"db", func(context.Context) error { return errors.New("down") }}, Check{"redis", func(context.Context) error { return nil }})
	rec = httptest.NewRecorder()
	bad.Handler()(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"db":"down"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadyTimeoutAndCacheExpiry(t *testing.T) {
	slow := NewReady(Options{Timeout: 20 * time.Millisecond}, Check{"s", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }})
	if _, ok := slow.Run(context.Background()); ok {
		t.Fatal("超时应判未就绪")
	}
	var n atomic.Int32
	r := NewReady(Options{TTL: time.Second}, Check{"a", func(context.Context) error { n.Add(1); return nil }})
	now := time.Now()
	r.now = func() time.Time { return now }
	r.Run(context.Background())
	now = now.Add(2 * time.Second)
	r.Run(context.Background())
	if n.Load() != 2 {
		t.Fatalf("缓存过期后应重查，次数=%d", n.Load())
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	Healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatal("healthz")
	}
}
