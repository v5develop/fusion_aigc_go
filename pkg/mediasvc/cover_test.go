package mediasvc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCoverURLReturnsObjectURL：媒体服务返回公有地址，CoverURL 原样返回，且写请求带 Idempotency-Key。
func TestCoverURLReturnsObjectURL(t *testing.T) {
	const want = "https://cdn.example.com/asset_library/sha256/x.jpg"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mediasvc/cover" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Idempotency-Key") == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":{"store":"asset_library","key":"sha256/x.jpg"},"url":{"kind":"public","value":"` + want + `"}}`))
	}))
	defer srv.Close()

	c := New([]string{srv.URL}, "tok")
	defer c.Close()
	got, err := c.CoverURL(context.Background(), "https://example.com/v.mp4")
	if err != nil {
		t.Fatalf("CoverURL error: %v", err)
	}
	if got != want {
		t.Fatalf("CoverURL = %s, want %s", got, want)
	}
}

// TestCoverURLWithoutReadableURLReturnsSentinel：没有可读地址时返回 ErrNoReadableURL，调用方按封面跳过处理。
func TestCoverURLWithoutReadableURLReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":{"store":"private","key":"k.jpg"},"url":{"kind":"none"}}`))
	}))
	defer srv.Close()

	c := New([]string{srv.URL}, "tok")
	defer c.Close()
	_, err := c.CoverURL(context.Background(), "https://example.com/v.mp4")
	if !errors.Is(err, ErrNoReadableURL) {
		t.Fatalf("CoverURL error = %v, want ErrNoReadableURL", err)
	}
}
