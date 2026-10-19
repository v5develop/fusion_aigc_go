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
	"syscall"
	"time"
)

// maxRedirects 与 Java 侧 FetchPolicy 的硬上限一致：再多就不是正常的 CDN 跳转。
const maxRedirects = 5

var errBlockedAddress = errors.New("地址不允许访问（内网 / 回环 / 保留地址）")

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
					return errors.New("重定向次数过多")
				}
				return checkScheme(req.URL)
			},
		},
		maxBytes: cfg.MaxInputBytes,
	}
}

// publicIP 只认公网单播地址。
func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast())
}

func checkScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return badInput("只支持 http / https 地址")
	}
	return nil
}

// download 把 rawURL 的内容存到 dir/name，返回本地路径。体积超限、地址不合规、源站 4xx 都是调用方的问题（422）；
// 超时、源站 5xx、网络错误是暂时的（503），调用方可以稍后重试。
func (f *fetcher) download(ctx context.Context, rawURL, dir, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", badInput("视频地址无效")
	}
	if err := checkScheme(u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", badInput("视频地址无效")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedAddress) {
			return "", badInput(errBlockedAddress.Error())
		}
		return "", unavailable(fmt.Sprintf("下载视频失败: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return "", unavailable(fmt.Sprintf("源站返回 %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return "", badInput(fmt.Sprintf("源站返回 %d", resp.StatusCode))
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
