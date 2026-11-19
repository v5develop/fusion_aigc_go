// Package safehttp 出站抓取用户可影响地址时的 SSRF 防护：建连时按实际解析出的 IP 拦内网 / 保留地址，
// 限制重定向，不走环境代理。media 的下载器、链接解析、广场转码等共用同一套判据。
package safehttp

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
)

// MaxRedirects 重定向硬上限：再多就不是正常的 CDN 跳转。
const MaxRedirects = 5

// ErrBlockedAddress 目标解析到内网 / 回环 / 保留地址。
var ErrBlockedAddress = errors.New("地址不允许访问（内网 / 回环 / 保留地址）")

// ErrRedirectForbidden 重定向到非 http(s) 协议或内网地址。
var ErrRedirectForbidden = errors.New("重定向到不允许的地址（协议或内网）")

// ErrTooManyRedirects 重定向次数过多。
var ErrTooManyRedirects = errors.New("重定向次数过多")

// reservedNets 不属于公网、但 net.IP 的 IsPrivate 不覆盖的保留段。
// 100.64.0.0/10（CGNAT）含阿里云 ECS 元数据地址 100.100.100.200，必须拦截，否则外部 URL 可读到实例凭证。
// IPv6 里：6to4（2002::/16）与 Teredo（2001::/32）把任意 IPv4 编码进地址，整段拒绝；
// 64:ff9b:1::/48 是本地用 NAT64 前缀，同样拒绝；知名 NAT64（64:ff9b::/96）单独取出内嵌 IPv4 再判。
var reservedNets = mustCIDRs(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"2002::/16", "2001::/32", "2001:db8::/32", "64:ff9b:1::/48", "fec0::/10", "100::/64",
)

var nat64Net = mustCIDRs("64:ff9b::/96")[0]

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

// PublicIP 只认公网单播地址。
func PublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// NAT64：真正要去的是内嵌的 IPv4，按它判（否则 64:ff9b::7f00:1 能绕过回环检查）
	if ip.To4() == nil && nat64Net.Contains(ip) {
		return PublicIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range reservedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// DialControl 作为 net.Dialer.Control：建连前对实际解析出的 IP 做检查，重定向与 DNS 重绑定都逃不掉。
func DialControl(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return ErrBlockedAddress
		}
		if !allowPrivate && !PublicIP(ip) {
			return ErrBlockedAddress
		}
		return nil
	}
}

// NewTransport 带 SSRF 拨号检查、不走环境代理的 Transport。
func NewTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: DialControl(allowPrivate)}
	return &http.Transport{
		DialContext: dialer.DialContext, Proxy: nil,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
	}
}

// CheckRedirect 返回重定向检查：次数上限、仅 http(s)、IP 字面量的内网目标当场拒绝（域名的在建连时拦）。
func CheckRedirect(allowPrivate bool) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return ErrTooManyRedirects
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return ErrRedirectForbidden
		}
		if ip := net.ParseIP(req.URL.Hostname()); ip != nil && !allowPrivate && !PublicIP(ip) {
			return ErrRedirectForbidden
		}
		return nil
	}
}

// NewClient 创建安全的 HTTP 客户端。allowPrivate 仅本地联调 / 测试用，线上必须 false。
func NewClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return &http.Client{
		Transport:     NewTransport(allowPrivate),
		Timeout:       timeout,
		CheckRedirect: CheckRedirect(allowPrivate),
	}
}

var (
	sharedOnce   sync.Once
	sharedClient *http.Client
)

// Shared 进程内共享的安全下载客户端（线上规则，不放行内网）。用于抓取用户可影响地址的素材；
// 总时长由调用方的 ctx 控制，这里只兜一个较宽的上限。
func Shared() *http.Client {
	sharedOnce.Do(func() { sharedClient = NewClient(10*time.Minute, false) })
	return sharedClient
}
