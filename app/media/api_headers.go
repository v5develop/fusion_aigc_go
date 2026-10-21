package media

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// 下载请求头（ingest 的 source.headers）。供应商素材地址有时需要鉴权或特定 UA。
//
// 约束：
//   - 只允许白名单头；白名单外的头（Cookie、Host、X-* 等）一律 400，不静默丢弃；
//   - 头只随原始请求发给 source.url 的来源，跳转到别的来源时剥掉（见 fetch.go 的 CheckRedirect）；
//   - 头的值不进入任何日志与错误信息，也不参与 rid / 幂等键的派生（rid 只由 Idempotency-Key 决定）；
//   - 进程内输入缓存的键包含头的摘要（sha256），所以带头下载的内容不会被不带头的请求命中。
const (
	maxHeaderValueBytes = 2048
	maxHeaderCount      = 8
)

var allowedSourceHeaders = map[string]bool{
	"Authorization":   true,
	"User-Agent":      true,
	"Accept":          true,
	"Referer":         true,
	"Accept-Language": true,
}

// validateHeaders 规范化并校验调用方给的头。错误的 check 只说明类别，不回显名字或值。
func validateHeaders(raw map[string]string) (http.Header, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxHeaderCount {
		return nil, invalid("source.headers_count")
	}
	out := make(http.Header, len(raw))
	for name, value := range raw {
		canon := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !allowedSourceHeaders[canon] {
			return nil, invalid("source.header_not_allowed")
		}
		if _, dup := out[canon]; dup {
			return nil, invalid("source.header_duplicate")
		}
		if len(value) > maxHeaderValueBytes || strings.ContainsAny(value, "\r\n\x00") {
			return nil, invalid("source.header_value")
		}
		out.Set(canon, value)
	}
	return out, nil
}

// headersDigest 头的摘要：只用于缓存隔离，不可逆，不写日志。无头返回空串。
func headersDigest(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, k := range names {
		sum.Write([]byte(k))
		sum.Write([]byte{0})
		sum.Write([]byte(h.Get(k)))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// cacheKeyFor 输入缓存的键：URL 加头摘要。无头时就是 URL 本身。
func cacheKeyFor(url string, h http.Header) string {
	if d := headersDigest(h); d != "" {
		return url + "\n#" + d
	}
	return url
}

// checkHeaderHost 调用方请求头只发给配置允许的素材主机。主机不在名单里（含名单为空）时拒绝，不发送凭证。
func (s *Server) checkHeaderHost(src SourceRef) error {
	if len(src.Headers) == 0 {
		return nil
	}
	u, err := url.Parse(src.Url)
	if err != nil || u.Hostname() == "" {
		return invalid("source.url")
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range strings.Split(s.svc.cfg.IngestHeaderHosts, ",") {
		if strings.ToLower(strings.TrimSpace(allowed)) == host && host != "" {
			return nil
		}
	}
	return newAPIError(codeForbiddenPurpose, "source.headers_host_not_allowed")
}
