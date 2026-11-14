package auth

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestWithRequestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		realIP     string
		xff        string
		want       string
	}{
		{"可信代理 + X-Real-IP", "127.0.0.1:5000", "203.0.113.7", "1.1.1.1, 203.0.113.7", "203.0.113.7"},
		{"可信代理无 X-Real-IP 取 XFF 末段，不取伪造的首段", "10.0.0.2:5000", "", "9.9.9.9, 203.0.113.8", "203.0.113.8"},
		{"直连（非可信对端）伪造头一律忽略", "198.51.100.5:4000", "9.9.9.9", "9.9.9.9", "198.51.100.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/auth/login", nil)
			r.RemoteAddr = c.remoteAddr
			if c.realIP != "" {
				r.Header.Set("X-Real-IP", c.realIP)
			}
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIPFrom(WithRequestClientIP(context.Background(), r)); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
	if got := clientIPFrom(context.Background()); got != "" {
		t.Fatalf("empty ctx got %q", got)
	}
}
