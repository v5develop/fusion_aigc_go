package logic

import "testing"

func TestMatchPrefixes(t *testing.T) {
	prefixes := []string{"https://cdn.example.com", "https://static.example.com/assets/"}
	cases := []struct {
		u    string
		want bool
	}{
		{"https://cdn.example.com/a.png", true},
		{"https://CDN.example.com/a.png", true},
		{"https://static.example.com/assets/x/a.png", true},
		{"https://cdn.example.com.evil.io/a.png", false},    // 前缀没带尾 / 的拼接
		{"https://cdn.example.com@evil.io/a.png", false},    // userinfo
		{"https://user:pw@cdn.example.com/a.png", false},    // userinfo
		{"https://cdn.example.com:8443/a.png", false},       // 端口不同
		{"http://cdn.example.com/a.png", false},             // 降级 http
		{"https://cdn.example.com/../x.png", false},         // .. 段
		{"https://cdn.example.com/a/%2e%2e/x.png", false},   // 编码的 .. 段
		{"https://cdn.example.com\\@evil.io/a.png", false},  // 反斜杠
		{"https://static.example.com/assetsX/a.png", false}, // 路径前缀须按段
		{"https://static.example.com/assets", false},        // 只有前缀本身
		{"ftp://cdn.example.com/a.png", false},
		{"//cdn.example.com/a.png", false},
		{"", false},
	}
	for _, c := range cases {
		if got := matchPrefixes(prefixes, c.u); got != c.want {
			t.Errorf("matchPrefixes(%q) = %v, want %v", c.u, got, c.want)
		}
	}
	if matchPrefixes([]string{"", "  ", "not a url"}, "https://cdn.example.com/a.png") {
		t.Error("无效前缀不应放行")
	}
}
