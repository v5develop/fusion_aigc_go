package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestFrom(t *testing.T) {
	cases := []struct {
		name, remote, realIP, xff, want string
	}{
		{"经 nginx：信 X-Real-IP", "127.0.0.1:5000", "203.0.113.9", "1.1.1.1, 203.0.113.9", "203.0.113.9"},
		{"经 nginx：XFF 首段伪造不影响", "127.0.0.1:5000", "203.0.113.9", "6.6.6.6", "203.0.113.9"},
		{"无 X-Real-IP：取 XFF 最后一段", "10.0.0.2:5000", "", "6.6.6.6, 203.0.113.9", "203.0.113.9"},
		{"直连（非可信对端）：头一律不信", "198.51.100.7:4000", "6.6.6.6", "6.6.6.6", "198.51.100.7"},
		{"X-Real-IP 非法：退到 XFF 尾段", "127.0.0.1:1", "not-an-ip", "203.0.113.9", "203.0.113.9"},
		{"全无头：取对端", "192.168.1.5:80", "", "", "192.168.1.5"},
		{"IPv6 对端回环", "[::1]:80", "2001:db8::1", "", "2001:db8::1"},
		{"IPv4 映射地址", "[::ffff:127.0.0.1]:80", "203.0.113.9", "", "203.0.113.9"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := From(r); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
