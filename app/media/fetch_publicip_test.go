package media

import (
	"net"
	"testing"
)

// 保留段（尤其是 CGNAT 与阿里云元数据地址）不是公网地址；真实公网地址仍然允许。
func TestPublicIPBlocksReservedRanges(t *testing.T) {
	blocked := []string{"100.100.100.200", "100.64.0.1", "127.0.0.1", "10.1.2.3", "192.168.1.1",
		"169.254.169.254", "0.1.2.3", "198.18.0.5", "240.0.0.1", "::1", "fd00::1"}
	for _, s := range blocked {
		if publicIP(net.ParseIP(s)) {
			t.Fatalf("%s 不应被当作公网地址", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "47.100.1.1"} {
		if !publicIP(net.ParseIP(s)) {
			t.Fatalf("%s 应当是公网地址", s)
		}
	}
}
