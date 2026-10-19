// Package mediasvc 是共享媒体服务（app/media）的 Go 调用方。契约见 docs/api/media-service.md。
//
// 实例来源有两种：静态地址列表，或从注册中心（r-nacos，Nacos 协议）订阅服务实例（媒体服务启动时自注册，退出时注销）。
// 请求按视频 URL 做 rendezvous 哈希排序，同一个视频总是先打到同一实例，让它的输入缓存生效；
// 首选实例连不上或返回 503 时转到下一个实例。
package mediasvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

// TokenHeader 与服务端的鉴权头一致。
const TokenHeader = "X-Media-Token"

const (
	maxRounds        = 3
	defaultWaitSec   = 5
	maxWaitSec       = 30
	discoveryTimeout = 5 * time.Second
)

// Client 调用媒体服务。
type Client struct {
	token string
	hc    *http.Client

	mu    sync.RWMutex
	bases []string

	// 注册中心订阅（为空则是静态地址）
	naming   naming_client.INamingClient
	service  string
	group    string
	fallback []string
}

// Error 服务端返回的错误。Permanent 为真时重试没有意义（输入问题、鉴权问题、部署问题）。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("mediasvc: %s (HTTP %d %s)", e.Message, e.Status, e.Code)
}

// Permanent 4xx 为永久失败；5xx 与网络错误是暂时的。
func (e *Error) Permanent() bool { return e.Status >= 400 && e.Status < 500 }

func newClient(token string) *Client {
	return &Client{token: token, hc: &http.Client{Timeout: 15 * time.Minute}}
}

// New 使用静态地址列表。地址为空返回 nil：未接入媒体服务时调用方自行降级。
func New(baseURLs []string, token string) *Client {
	bases := normalize(baseURLs)
	if len(bases) == 0 {
		return nil
	}
	c := newClient(token)
	c.bases = bases
	return c
}

// DiscoveryConfig 注册中心发现配置。ServerAddr 为空则不做发现，只用静态地址。
type DiscoveryConfig struct {
	// ServerAddr r-nacos 的 HTTP 地址 host:port（gRPC 端口自动为 +1000）
	ServerAddr  string
	Namespace   string
	Group       string
	ServiceName string
	Username    string
	Password    string
}

// Discover 订阅注册中心里的媒体服务实例（非阻塞）。启动时注册中心不可用不会失败：
// 连上之后订阅自动生效；注册中心没有健康实例时使用 fallback。
func Discover(dc DiscoveryConfig, token string, fallback []string) *Client {
	c := newClient(token)
	c.fallback = normalize(fallback)
	c.bases = c.fallback
	if strings.TrimSpace(dc.ServerAddr) == "" {
		return c
	}
	if dc.Group == "" {
		dc.Group = "DEFAULT_GROUP"
	}
	if dc.ServiceName == "" {
		dc.ServiceName = "aigc-media"
	}
	naming, err := newNamingClient(dc)
	if err != nil {
		log.Printf("mediasvc: 注册中心初始化失败，暂用静态地址: %v", err)
		return c
	}
	c.naming, c.service, c.group = naming, dc.ServiceName, dc.Group

	// 启动时先拉一次健康实例，之后由订阅回调持续更新
	if list, err := naming.SelectInstances(vo.SelectInstancesParam{
		ServiceName: c.service, GroupName: c.group, HealthyOnly: true,
	}); err == nil {
		c.setInstances(list)
	}
	if err := naming.Subscribe(&vo.SubscribeParam{
		ServiceName: c.service,
		GroupName:   c.group,
		SubscribeCallback: func(services []model.Instance, err error) {
			if err != nil {
				log.Printf("mediasvc: 订阅回调错误: %v", err)
				return
			}
			c.setInstances(services)
		},
	}); err != nil {
		log.Printf("mediasvc: 订阅注册中心失败，仅使用启动时的实例列表: %v", err)
	}
	return c
}

// setInstances 把注册中心的实例换算成调用地址。只要健康且启用的实例；没有则回退到静态地址。
func (c *Client) setInstances(services []model.Instance) {
	var found []string
	for _, inst := range services {
		if !inst.Healthy || !inst.Enable {
			continue
		}
		scheme := inst.Metadata["scheme"]
		if scheme == "" {
			scheme = "http"
		}
		found = append(found, fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(inst.Ip, strconv.FormatUint(uint64(inst.Port), 10))))
	}
	bases := normalize(found)
	if len(bases) == 0 {
		bases = c.fallback
	}
	c.mu.Lock()
	c.bases = bases
	c.mu.Unlock()
}

// Close 取消订阅并关闭注册中心客户端。
func (c *Client) Close() {
	if c == nil || c.naming == nil {
		return
	}
	_ = c.naming.Unsubscribe(&vo.SubscribeParam{ServiceName: c.service, GroupName: c.group})
	c.naming.CloseClient()
}

// Instances 当前可用的实例地址（测试与排障用）。
func (c *Client) Instances() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.bases...)
}

func normalize(urls []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range urls {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// rank 按 key 给实例排序（rendezvous 哈希）。实例增减时只有少量 key 会换实例。
func (c *Client) rank(key string) []string {
	c.mu.RLock()
	bases := append([]string(nil), c.bases...)
	c.mu.RUnlock()
	type scored struct {
		base  string
		score *big.Int
	}
	list := make([]scored, 0, len(bases))
	for _, b := range bases {
		sum := sha256.Sum256([]byte(b + "|" + key))
		list = append(list, scored{b, new(big.Int).SetBytes(sum[:])})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].score.Cmp(list[j].score) > 0 })
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.base
	}
	return out
}

// Cover 抽一张代表帧（JPEG 字节）。路由 key 是视频 URL。
func (c *Client) Cover(ctx context.Context, videoURL string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"video_url": videoURL})
	if err != nil {
		return nil, err
	}
	return c.post(ctx, videoURL, "/v1/cover", body, "application/json")
}

// post 按路由顺序发送：连不上或 503 转到下一个实例；整轮都是 503 时等待后再来一轮。
func (c *Client) post(ctx context.Context, key, path string, body []byte, contentType string) ([]byte, error) {
	var last error
	for round := 1; round <= maxRounds; round++ {
		overloaded := false
		wait := defaultWaitSec
		bases := c.rank(key)
		if len(bases) == 0 {
			return nil, &Error{Status: http.StatusServiceUnavailable, Code: "no_instance", Message: "没有可用的媒体服务实例"}
		}
		for _, base := range bases {
			out, status, retryAfter, err := c.do(ctx, base+path, body, contentType)
			if err != nil {
				last = &Error{Status: http.StatusServiceUnavailable, Code: "unreachable", Message: "媒体服务暂不可用"}
				continue
			}
			if status == http.StatusOK {
				return out, nil
			}
			last = errorFrom(status, out)
			if status != http.StatusServiceUnavailable {
				return nil, last
			}
			overloaded = true
			wait = max(wait, retryAfter)
		}
		if !overloaded || round == maxRounds {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(min(wait, maxWaitSec)) * time.Second):
		}
	}
	return nil, last
}

func (c *Client) do(ctx context.Context, url string, body []byte, contentType string) ([]byte, int, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(TokenHeader, c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, err
	}
	return out, resp.StatusCode, retryAfterSeconds(resp.Header.Get("Retry-After")), nil
}

func retryAfterSeconds(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return defaultWaitSec
	}
	return min(n, maxWaitSec)
}

func errorFrom(status int, body []byte) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Message == "" {
		e.Message = fmt.Sprintf("媒体服务返回 %d", status)
	}
	return &Error{Status: status, Code: e.Code, Message: e.Message}
}

// newNamingClient 创建注册中心客户端。
func newNamingClient(dc DiscoveryConfig) (naming_client.INamingClient, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(dc.ServerAddr))
	if err != nil {
		return nil, fmt.Errorf("注册中心地址须为 host:port: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("注册中心端口无效: %w", err)
	}
	return clients.NewNamingClient(vo.NacosClientParam{
		ClientConfig: &constant.ClientConfig{
			NamespaceId:         dc.Namespace,
			TimeoutMs:           5000,
			NotLoadCacheAtStart: true,
			Username:            dc.Username,
			Password:            dc.Password,
			LogLevel:            "warn",
			LogDir:              filepath.Join(os.TempDir(), "aigc-nacos-log"),
			CacheDir:            filepath.Join(os.TempDir(), "aigc-nacos-cache"),
		},
		ServerConfigs: []constant.ServerConfig{{IpAddr: host, Port: port}},
	})
}
