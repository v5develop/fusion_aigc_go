package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// maxRedirects 与 Java 侧 FetchPolicy 的硬上限一致：再多就不是正常的 CDN 跳转。
const maxRedirects = 5

var errBlockedAddress = errors.New("地址不允许访问（内网 / 回环 / 保留地址）")

// 重定向的永久错误（素材地址本身的问题，重试没有意义）：经 client.Do 返回时包在 *url.Error 里，用 errors.Is 识别。
var (
	errTooManyRedirects  = errors.New("重定向次数过多")
	errRedirectForbidden = errors.New("重定向到不允许的地址（协议或内网）")
)

// redirectTraceKey 请求上下文里挂一个 *redirectTrace，CheckRedirect 记下是否发生过跳转：
// 建连时被拦的内网地址（DNS 解析后才知道）如果出现在跳转之后，按"重定向到被禁止的地址"归类。
type redirectTraceKey struct{}

type redirectTrace struct{ hops atomic.Int32 }

// fetcher 下载输入视频。防 SSRF 的要点：
//   - 只放行 http / https；
//   - 在建连时（Control 钩子）对实际解析出的 IP 做检查，重定向和 DNS 重绑定都逃不掉；
//   - 不走环境变量里的代理（代理地址本身可能是内网）。
type fetcher struct {
	client   *http.Client
	maxBytes int64
}

func newFetcher(cfg Config) *fetcher {
	dialer := &net.Dialer{
		Timeout: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return errBlockedAddress
			}
			if !cfg.AllowPrivateHosts && !publicIP(ip) {
				return errBlockedAddress
			}
			return nil
		},
	}
	transport := &http.Transport{DialContext: dialer.DialContext, Proxy: nil}
	return &fetcher{
		client: &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.FetchTimeoutSeconds) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return errTooManyRedirects
				}
				if rt, ok := req.Context().Value(redirectTraceKey{}).(*redirectTrace); ok {
					rt.hops.Add(1)
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return errRedirectForbidden
				}
				// 跳转目标直接写成 IP 的，不必等到建连：内网 / 保留地址当场拒绝（域名的在 Dial 的 Control 里拦）
				if ip := net.ParseIP(req.URL.Hostname()); ip != nil && !cfg.AllowPrivateHosts && !publicIP(ip) {
					return errRedirectForbidden
				}
				// 带请求头的下载：跳到别的来源（主机或协议不同）时，调用方的头一律剥掉，只保留同来源的跳转
				if !sameOrigin(via[0].URL, req.URL) {
					req.Header = http.Header{}
				}
				return nil
			},
		},
		maxBytes: cfg.MaxInputBytes,
	}
}

// reservedNets 不属于公网、但 net.IP 的 IsPrivate 不覆盖的保留段。
// 其中 100.64.0.0/10（CGNAT）包含阿里云 ECS 元数据地址 100.100.100.200，必须拦截，否则外部 URL 可以读到实例凭证。
var reservedNets = mustCIDRs(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
)

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

// publicIP 只认公网单播地址。
func publicIP(ip net.IP) bool {
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

// sameOrigin 协议与主机（含端口）都相同。
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Host == b.Host
}

func checkScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return badInput("只支持 http / https 地址")
	}
	return nil
}

// transientUpstreamStatus 源站这几个 4xx 是暂时的：限流（429）、请求超时（408），以及 CDN / 对象存储
// 在签名刚过期或被临时拦截时返回的 403。按瞬时处理（503），调用方用同一键重试，避免把可恢复的素材判成永久失败。
func transientUpstreamStatus(code int) bool {
	return code == http.StatusForbidden || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
}

// classifyFetchError 分类 client.Do 的错误。重定向次数过多、重定向到被禁止的地址（非 http/https 协议、内网 / 保留地址）
// 是素材地址本身的问题，判永久（422 source_unavailable）；首跳就是内网地址保持原分类（422 source_rejected）；
// 其余（超时、连接被拒或中断、DNS 临时失败、TLS 握手失败）仍是瞬时（503 transient）。
func classifyFetchError(err error, redirected bool) error {
	switch {
	case errors.Is(err, errTooManyRedirects):
		return sourceUnavailable("source.redirect_too_many", errTooManyRedirects.Error())
	case errors.Is(err, errRedirectForbidden):
		return sourceUnavailable("source.redirect_forbidden", errRedirectForbidden.Error())
	case errors.Is(err, errBlockedAddress):
		if redirected {
			return sourceUnavailable("source.redirect_forbidden", errRedirectForbidden.Error())
		}
		return badInput(errBlockedAddress.Error())
	}
	return unavailable(fmt.Sprintf("下载视频失败: %v", err))
}

// maxRetryAfterSeconds 透传给调用方的 Retry-After 上限（秒）。
// 截断理由：Retry-After 由源站（第三方 CDN / 对象存储）控制，不受信任；调用方（Java 工作流）按它推迟重试，
// 过大的值会把任务挂住远超一次处理的排队上限（queueWaitCap 同为 120 秒），恶意或配置错误的源站可借此拖住任务。
// 超过 120 秒的冷却交给调用方自己的退避策略。
const maxRetryAfterSeconds = 120

// parseRetryAfter 解析 Retry-After（RFC 9110 §10.2.3：非负整数秒，或 HTTP 日期），返回秒数，截到 [1, 120]。
// 没有或格式非法返回 0（由 writeAPIError 回落默认 5 秒）。0 秒或已过去的日期按 1 秒，避免调用方零间隔重试。
func parseRetryAfter(v string, now time.Time) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var secs int64
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		secs = n
	} else if errors.Is(err, strconv.ErrRange) && v[0] != '-' {
		secs = maxRetryAfterSeconds // 超出 int64 的秒数：按上限
	} else if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		secs = int64((d + time.Second - 1) / time.Second) // 向上取整，不早于源站要求的时刻
	} else {
		return 0
	}
	switch {
	case secs < 1:
		return 1
	case secs > maxRetryAfterSeconds:
		return maxRetryAfterSeconds
	}
	return int(secs)
}

// download 把 rawURL 的内容存到 dir/name，返回本地路径。
// 分类：体积超限、地址不合规（422 source_rejected）；源站 404 / 410 与其它 4xx（除上面几个）为 source_unavailable（422，永久）；
// 重定向次数过多、重定向到被禁止的地址为 source_unavailable（422，永久，见 classifyFetchError）；
// 源站 5xx、429、408、403、超时、连接中断、DNS 临时失败为 transient（503，可重试），源站的 Retry-After 透传（见 parseRetryAfter）。
func (f *fetcher) download(ctx context.Context, rawURL, dir, name string) (string, error) {
	return f.downloadWithHeaders(ctx, rawURL, nil, dir, name)
}

// downloadWithHeaders 同 download，并随原始请求发送 headers（已经过 validateHeaders 白名单校验）。
// 头只随 rawURL 的来源发送；跳转到别的来源时由 CheckRedirect 剥掉。错误信息与日志里都不出现头的值。
func (f *fetcher) downloadWithHeaders(ctx context.Context, rawURL string, headers http.Header, dir, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", badInput("视频地址无效")
	}
	if err := checkScheme(u); err != nil {
		return "", err
	}
	trace := &redirectTrace{}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, redirectTraceKey{}, trace), http.MethodGet, rawURL, nil)
	if err != nil {
		return "", badInput("视频地址无效")
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", classifyFetchError(err, trace.hops.Load() > 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || transientUpstreamStatus(resp.StatusCode) {
		return "", unavailableRetryAfter(fmt.Sprintf("源站返回 %d", resp.StatusCode),
			parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()))
	}
	if resp.StatusCode != http.StatusOK {
		return "", upstreamBadInput(resp.StatusCode, fmt.Sprintf("源站返回 %d", resp.StatusCode))
	}
	if resp.ContentLength > f.maxBytes {
		return "", badInput("视频体积超过上限")
	}

	path := filepath.Join(dir, name)
	out, err := os.Create(path)
	if err != nil {
		return "", internal(err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(resp.Body, f.maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return "", unavailable(fmt.Sprintf("下载视频中断: %v", copyErr))
	}
	if closeErr != nil {
		return "", internal(closeErr)
	}
	if n > f.maxBytes {
		return "", badInput("视频体积超过上限")
	}
	return path, nil
}
