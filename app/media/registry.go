package media

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

// RegistryConfig 注册中心（r-nacos / Nacos 协议）配置。ServerAddr 为空则不注册（单实例或静态列表部署）。
type RegistryConfig struct {
	// ServerAddr r-nacos 的 HTTP 地址 host:port（gRPC 端口自动为 +1000，即 9848）
	ServerAddr string `json:",optional"`
	// Namespace 命名空间，留空为 public
	Namespace string `json:",optional"`
	// Group 服务分组
	Group string `json:",default=DEFAULT_GROUP"`
	// ServiceName 服务名：调用方按同一个名字订阅
	ServiceName string `json:",default=aigc-media"`
	// Username / Password 注册中心开启鉴权时填写
	Username string `json:",optional"`
	Password string `json:",optional"`
	// Advertise 本实例对外的基础地址，如 http://10.0.0.5:18090。注册中心要求 IP，不能是域名。
	Advertise string `json:",optional"`
}

// Register 把本实例注册为临时实例（ephemeral）。SDK 通过 gRPC 连接保活：进程退出或断线后注册中心自动摘除，
// 不需要自己续约。连不上注册中心时后台退避重试，不阻塞服务启动。
// 返回的函数用于优雅退出：先注销实例（调用方立刻停止路由到本实例），再关闭客户端。
func Register(ctx context.Context, cfg RegistryConfig) (deregister func(), err error) {
	if strings.TrimSpace(cfg.ServerAddr) == "" {
		return func() {}, nil
	}
	inst, err := instanceOf(cfg.Advertise)
	if err != nil {
		return nil, err
	}
	client, err := newNamingClient(cfg.ServerAddr, cfg.Namespace, cfg.Username, cfg.Password)
	if err != nil {
		return nil, err
	}
	param := vo.RegisterInstanceParam{
		Ip:          inst.ip,
		Port:        inst.port,
		Weight:      1,
		Enable:      true,
		Healthy:     true,
		Ephemeral:   true,
		ServiceName: cfg.ServiceName,
		GroupName:   cfg.Group,
		Metadata:    map[string]string{"scheme": inst.scheme},
	}
	dereg := vo.DeregisterInstanceParam{
		Ip:          inst.ip,
		Port:        inst.port,
		ServiceName: cfg.ServiceName,
		GroupName:   cfg.Group,
		Ephemeral:   true,
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		backoff := time.Second
		for {
			ok, err := client.RegisterInstance(param)
			if err == nil && ok {
				log.Printf("media: 已注册到注册中心 service=%s instance=%s:%d", cfg.ServiceName, inst.ip, inst.port)
				break
			}
			log.Printf("media: 注册中心注册失败，%s 后重试: %v", backoff, err)
			select {
			case <-runCtx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
		<-runCtx.Done()
		if _, err := client.DeregisterInstance(dereg); err != nil {
			log.Printf("media: 注销实例失败（临时实例会随连接断开自动摘除）: %v", err)
		}
	}()

	return func() {
		cancel()
		<-done
		client.CloseClient()
	}, nil
}

// instance 注册中心里一个实例：IP、端口、调用 scheme。
type instance struct {
	ip     string
	port   uint64
	scheme string
}

// instanceOf 从 Advertise 地址解析出注册需要的 IP 与端口。注册中心只接受 IP，域名在这里直接拒绝。
func instanceOf(advertise string) (instance, error) {
	u, err := url.Parse(strings.TrimSpace(advertise))
	if err != nil || u.Host == "" {
		return instance{}, errors.New("media: MEDIA_ADVERTISE_URL 未设置或无效（示例 http://10.0.0.5:18090）：注册中心只接受 IP，且必须是调用方能直连的地址")
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return instance{}, fmt.Errorf("media: Advertise 必须带端口: %w", err)
	}
	if net.ParseIP(host) == nil {
		return instance{}, fmt.Errorf("media: Advertise 必须是 IP，不能是域名: %s", host)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return instance{}, fmt.Errorf("media: Advertise 端口无效: %w", err)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return instance{ip: host, port: port, scheme: scheme}, nil
}

// newNamingClient 创建 r-nacos 的命名服务客户端。ServerAddr 的 HTTP 端口给 SDK，gRPC 端口由 SDK 按 +1000 推算。
func newNamingClient(serverAddr, namespace, username, password string) (naming_client.INamingClient, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(serverAddr))
	if err != nil {
		return nil, fmt.Errorf("注册中心地址须为 host:port: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("注册中心端口无效: %w", err)
	}
	return clients.NewNamingClient(vo.NacosClientParam{
		ClientConfig: &constant.ClientConfig{
			NamespaceId:         namespace,
			TimeoutMs:           5000,
			NotLoadCacheAtStart: true,
			Username:            username,
			Password:            password,
			LogLevel:            "warn",
			LogDir:              filepath.Join(os.TempDir(), "aigc-nacos-log"),
			CacheDir:            filepath.Join(os.TempDir(), "aigc-nacos-cache"),
		},
		ServerConfigs: []constant.ServerConfig{{IpAddr: host, Port: port}},
	})
}
