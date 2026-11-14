// Package clientip 从 HTTP 请求里取调用方真实 IP（匿名限频 / 去重 / 审计用）。
//
// 规则：只信「最近一跳可信代理」写入的头。
//
// 入口 nginx（deploy/server/nginx/aigc-local-life.conf.tmpl；fusion-aigc deploy/gateway/proxy-base.conf、
// baota/*.conf 同口径）对上游写的是：
//
//	proxy_set_header X-Real-IP       $remote_addr;               # 覆盖写：nginx 看到的对端地址，客户端伪造的同名头被替换
//	proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for; # 追加写：客户端自带的 XFF 原样保留在前面
//
// 所以 X-Forwarded-For 的首段是客户端随便填的（伪造一个就能绕过按 IP 的限频），不能信；
// X-Real-IP 由最近一跳 nginx 用 $remote_addr 覆盖，只有当直连对端（r.RemoteAddr）本身是可信代理时才可信——
// Go 服务监听 0.0.0.0，如果有人绕过 nginx 直连端口，他自带的 X-Real-IP 同样是伪造的，此时只认 r.RemoteAddr。
// 没有 X-Real-IP 时取 X-Forwarded-For 的**最后一段**（最近一跳代理追加的那个，即它看到的 $remote_addr）。
//
// 可信代理默认是回环与内网段（nginx 与服务同机 127.0.0.1，或同一 docker 网络 / 内网）。
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultTrusted 默认可信代理网段：回环、RFC1918 内网、IPv6 ULA / 链路本地。
var DefaultTrusted = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// From 用 DefaultTrusted 取调用方 IP。
func From(r *http.Request) string { return FromTrusted(r, DefaultTrusted) }

// FromTrusted 直连对端在 trusted 内才看代理头（X-Real-IP 优先，其次 X-Forwarded-For 最后一段），否则返回直连对端地址。
// 返回值不含端口；头里的值不是合法 IP 时忽略该头。
func FromTrusted(r *http.Request, trusted []netip.Prefix) string {
	peer := hostOnly(r.RemoteAddr)
	pa, err := netip.ParseAddr(peer)
	if err != nil || !inAny(pa.Unmap(), trusted) {
		return peer
	}
	if v := validIP(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if v := validIP(parts[len(parts)-1]); v != "" {
			return v
		}
	}
	return peer
}

func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

func validIP(s string) string {
	s = hostOnly(s)
	a, err := netip.ParseAddr(s)
	if err != nil {
		return ""
	}
	return a.Unmap().String()
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
