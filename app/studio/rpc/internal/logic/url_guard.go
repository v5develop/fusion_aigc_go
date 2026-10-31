package logic

import (
	"net/url"
	"strings"
)

// 输入图地址白名单校验（与 assetlib 的 matchPrefixes / cleanHTTPURL 同口径；跨服务 internal 包不能引用，这里复制一份）。

// cleanHTTPURL 解析后是 http(s)、有 host、不带账号信息（user@host）、没有 .. 路径段（含 %2e%2e 解码后）、不含反斜杠。
// 防纯字符串前缀比对被花式拼接绕过。
func cleanHTTPURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil || strings.Contains(u, "\\") {
		return false
	}
	for _, seg := range strings.Split(p.Path, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// matchPrefixes u 命中任一前缀：scheme + host（含端口）完全相等，路径以前缀路径（自动补尾 /）开头。
// 不能用字符串 HasPrefix：前缀没带尾部 / 时 https://cdn.example.com 会放过 https://cdn.example.com.evil.io。
func matchPrefixes(prefixes []string, u string) bool {
	if !cleanHTTPURL(u) {
		return false
	}
	pu, _ := url.Parse(u)
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pp, err := url.Parse(p)
		if err != nil || pp.Host == "" || pp.User != nil {
			continue
		}
		if !strings.EqualFold(pp.Scheme, pu.Scheme) || !strings.EqualFold(pp.Host, pu.Host) {
			continue
		}
		base := strings.TrimRight(pp.Path, "/") + "/"
		if strings.HasPrefix(pu.Path, base) {
			return true
		}
	}
	return false
}
