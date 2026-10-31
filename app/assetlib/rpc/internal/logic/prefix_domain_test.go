package logic

import "testing"

// 白名单允许只写域名：缺协议按 https，路径按段匹配。
func TestMatchPrefixesBareDomain(t *testing.T) {
	if !matchPrefixes([]string{"cdn.example.com"}, "https://cdn.example.com/a/b.png") {
		t.Error("裸域名应放行该域任意路径")
	}
	if !matchPrefixes([]string{"assets.example.com/lib"}, "https://assets.example.com/lib/x.png") {
		t.Error("裸域名+路径应按段放行")
	}
	if matchPrefixes([]string{"assets.example.com/lib"}, "https://assets.example.com/library/x.png") {
		t.Error("路径段不完整不应放行")
	}
	if matchPrefixes([]string{"cdn.example.com"}, "http://cdn.example.com/a.png") {
		t.Error("裸域名按 https，http 图片不应放行")
	}
}
