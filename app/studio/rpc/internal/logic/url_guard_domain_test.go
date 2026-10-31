package logic

import "testing"

// 白名单允许只写域名：缺协议按 https，路径按段匹配，协议与主机必须一致。
func TestMatchPrefixesBareDomain(t *testing.T) {
	cases := []struct {
		name     string
		prefixes []string
		u        string
		want     bool
	}{
		{"裸域名放行该域任意路径", []string{"cdn.example.com"}, "https://cdn.example.com/a/b.png", true},
		{"裸域名+路径按段放行", []string{"assets.example.com/lib"}, "https://assets.example.com/lib/x.png", true},
		{"路径段不完整不放行", []string{"assets.example.com/lib"}, "https://assets.example.com/library/x.png", false},
		{"裸域名不放行其它域", []string{"cdn.example.com"}, "https://evil.example.com/a.png", false},
		{"裸域名按 https 处理，http 图片不放行", []string{"cdn.example.com"}, "http://cdn.example.com/a.png", false},
		{"已写协议的条目保持原样", []string{"http://cdn.example.com"}, "http://cdn.example.com/a.png", true},
		{"空白与空条目忽略", []string{"  ", "cdn.example.com "}, "https://cdn.example.com/a.png", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchPrefixes(c.prefixes, c.u); got != c.want {
				t.Errorf("matchPrefixes(%v, %q) = %v, want %v", c.prefixes, c.u, got, c.want)
			}
		})
	}
}
