package media

import (
	"context"
	"crypto/md5"
	"encoding/hex"
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
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
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

// instanceIDMetaKey 实例元数据里记录实例 ID：调用方据此把 rid 路由到它的所属实例。
const instanceIDMetaKey = "instance_id"

// tombstoneDataID 下线实例 ID 的墓碑列表（注册中心的配置项，每行一个 ID，# 开头为注释）。
const tombstoneDataID = "media-instance-tombstones"

var (
	// ErrInstanceIDInUse 同一个实例 ID 已有存活实例在别的地址上运行：拒绝启动。
	ErrInstanceIDInUse = errors.New("media: 实例 ID 已被其它存活实例占用，拒绝启动（每个部署位置必须有自己的 InstanceID）")
	// ErrInstanceIDRetired 实例 ID 已下线（墓碑），永不复用：拒绝启动。
	ErrInstanceIDRetired = errors.New("media: 实例 ID 已下线（墓碑），不能复用，请生成新的 InstanceID")
)

// IsRefusedRegistration 注册被拒绝（ID 冲突或已下线）：调用方应拒绝启动，而不是后台重试。
func IsRefusedRegistration(err error) bool {
	return errors.Is(err, ErrInstanceIDInUse) || errors.Is(err, ErrInstanceIDRetired)
}

// instanceRecheckInterval 注册成功后复查实例 ID 的间隔。注册前的检查与注册之间有窗口，别的实例可能在此期间抢到同一个 ID。
var instanceRecheckInterval = 30 * time.Second

// tombstoneCASAttempts 墓碑写入的最大尝试次数（读 md5 → 带 md5 写 → 失败则重读重试）。
const tombstoneCASAttempts = 3

// checkInstanceID 检查注册中心里是否有别的地址在用同一个 ID。同一地址（本实例重启）允许。
// 调用方传入的是全部注册实例（含暂时不健康的：它的心跳可能刚断，ID 仍被占用）；这里忽略被管理员禁用（enable=false）的实例。
func checkInstanceID(registered []model.Instance, instanceID string, self instance) error {
	for _, in := range registered {
		if !in.Enable || in.Metadata[instanceIDMetaKey] != instanceID {
			continue
		}
		if in.Ip == self.ip && in.Port == self.port {
			continue
		}
		return fmt.Errorf("%w（占用方 %s:%d）", ErrInstanceIDInUse, in.Ip, in.Port)
	}
	return nil
}

// tombstoned 墓碑列表里是否有该 ID。每行一个 ID，空行与 # 开头的注释忽略。
func tombstoned(list, instanceID string) bool {
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == instanceID {
			return true
		}
	}
	return false
}

// readTombstones 读墓碑列表。配置项还不存在（注册中心里从未下线过实例）时视为空列表。
func readTombstones(config config_client.IConfigClient, group string) (string, error) {
	list, _, err := readTombstonesCAS(config, group)
	return list, err
}

// readTombstonesCAS 读墓碑列表并返回内容的 md5（写入时作为 CasMd5 带上）。
// 配置项不存在时返回空列表与空 md5：空 md5 表示不做 CAS 比较，写入时若别人已经建了配置项，会 CAS 失败并重读。
func readTombstonesCAS(config config_client.IConfigClient, group string) (list, casMd5 string, err error) {
	list, err = config.GetConfig(vo.ConfigParam{DataId: tombstoneDataID, Group: group})
	if err != nil {
		// SDK 对不存在的配置项返回 "config data not exist"：这是正常的空状态，不是读取失败
		if strings.Contains(err.Error(), "config data not exist") {
			return "", "", nil
		}
		return "", "", fmt.Errorf("读取墓碑列表失败: %w", err)
	}
	sum := md5.Sum([]byte(list))
	return list, hex.EncodeToString(sum[:]), nil
}

// registeredInstances 查询注册中心里该服务的全部实例（健康与否都要）。
// 实例 ID 冲突检查必须看全部实例：心跳刚断的实例仍占着 ID，只看健康实例会让新实例抢到同一个 ID。
// 注意：SelectInstances(HealthyOnly=false) 在 SDK 里是"只返回不健康的"，不能用来取全部实例，所以用 SelectAllInstances。
func registeredInstances(naming naming_client.INamingClient, cfg RegistryConfig) ([]model.Instance, error) {
	all, err := naming.SelectAllInstances(vo.SelectAllInstancesParam{
		ServiceName: cfg.ServiceName, GroupName: cfg.Group,
	})
	if err != nil {
		return nil, fmt.Errorf("查询注册实例失败: %w", err)
	}
	return all, nil
}

// liveInstances 查询存活（健康）实例，用于选 owner。服务还没有任何实例时 SDK 返回 "instance list is empty"：这是正常的空状态。
func liveInstances(naming naming_client.INamingClient, cfg RegistryConfig) ([]model.Instance, error) {
	live, err := naming.SelectInstances(vo.SelectInstancesParam{
		ServiceName: cfg.ServiceName, GroupName: cfg.Group, HealthyOnly: true,
	})
	if err != nil {
		if strings.Contains(err.Error(), "instance list is empty") {
			return nil, nil
		}
		return nil, fmt.Errorf("查询存活实例失败: %w", err)
	}
	return live, nil
}

// registryClients 注册中心的命名服务（注册、查询实例）与配置服务（墓碑列表）。
type registryClients struct {
	naming naming_client.INamingClient
	config config_client.IConfigClient
}

func newRegistryClients(cfg RegistryConfig) (registryClients, error) {
	param, err := nacosParam(cfg.ServerAddr, cfg.Namespace, cfg.Username, cfg.Password)
	if err != nil {
		return registryClients{}, err
	}
	naming, err := clients.NewNamingClient(param)
	if err != nil {
		return registryClients{}, err
	}
	config, err := clients.NewConfigClient(param)
	if err != nil {
		naming.CloseClient()
		return registryClients{}, err
	}
	return registryClients{naming: naming, config: config}, nil
}

func (rc registryClients) close() {
	rc.naming.CloseClient()
	rc.config.CloseClient()
}

// precheck 注册前检查：墓碑（致命）、注册中心里同 ID 的其它地址实例（致命）。连接失败等返回普通错误，由调用方重试。
func precheck(rc registryClients, cfg RegistryConfig, instanceID string, self instance) error {
	list, err := readTombstones(rc.config, cfg.Group)
	if err != nil {
		return err
	}
	if tombstoned(list, instanceID) {
		return ErrInstanceIDRetired
	}
	registered, err := registeredInstances(rc.naming, cfg)
	if err != nil {
		return err
	}
	return checkInstanceID(registered, instanceID, self)
}

// Register 把本实例注册为临时实例（ephemeral），启动前先检查实例 ID：
//   - 墓碑或存活同 ID 实例：同步返回 ErrInstanceIDRetired / ErrInstanceIDInUse，调用方拒绝启动；
//   - 注册中心暂时连不上：后台退避重试，重试前每次都重新检查，发现冲突时调用 onFatal。
//
// SDK 通过 gRPC 连接保活：进程退出或断线后注册中心自动摘除，不需要自己续约。
// 返回的函数用于优雅退出：先注销实例（调用方立刻停止路由到本实例），再关闭客户端。
func Register(ctx context.Context, cfg RegistryConfig, instanceID string, onFatal func(error)) (deregister func(), err error) {
	if strings.TrimSpace(cfg.ServerAddr) == "" {
		return func() {}, nil
	}
	inst, err := instanceOf(cfg.Advertise)
	if err != nil {
		return nil, err
	}
	rc, err := newRegistryClients(cfg)
	if err != nil {
		return nil, err
	}
	if err := precheck(rc, cfg, instanceID, inst); IsRefusedRegistration(err) {
		rc.close()
		return nil, err
	} else if err != nil {
		log.Printf("media: 启动前检查注册中心失败，后台重试时再检查: %v", err)
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
		Metadata:    map[string]string{"scheme": inst.scheme, instanceIDMetaKey: instanceID},
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
			err := precheck(rc, cfg, instanceID, inst)
			if IsRefusedRegistration(err) {
				log.Printf("media: 注册被拒绝，停止注册: %v", err)
				if onFatal != nil {
					onFatal(err)
				}
				return
			}
			if err == nil {
				ok, regErr := rc.naming.RegisterInstance(param)
				if regErr == nil && ok {
					log.Printf("media: 已注册到注册中心 service=%s instance=%s:%d id=%s", cfg.ServiceName, inst.ip, inst.port, instanceID)
					break
				}
				err = regErr
				if err == nil {
					err = errors.New("注册中心未确认注册")
				}
			}
			log.Printf("media: 注册中心注册失败，%s 后重试: %v", backoff, err)
			select {
			case <-runCtx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
		// 注册成功后定期复查：检查与注册之间的窗口里，别的地址可能已经占用了同一个 ID
		ticker := time.NewTicker(instanceRecheckInterval)
		defer ticker.Stop()
	watch:
		for {
			select {
			case <-runCtx.Done():
				break watch
			case <-ticker.C:
				err := precheck(rc, cfg, instanceID, inst)
				if IsRefusedRegistration(err) {
					log.Printf("media: 注册后复查发现实例 ID 冲突: %v", err)
					if onFatal != nil {
						onFatal(err)
					}
					break watch
				}
				if err != nil {
					log.Printf("media: 注册后复查失败，下个周期再查: %v", err)
				}
			}
		}
		if _, err := rc.naming.DeregisterInstance(dereg); err != nil {
			log.Printf("media: 注销实例失败（临时实例会随连接断开自动摘除）: %v", err)
		}
	}()

	return func() {
		cancel()
		<-done
		rc.close()
	}, nil
}

// Retire 把实例 ID 写入墓碑列表：此后这个 ID 永不能再注册（下线时运维执行，见 cmd 的 -retire）。
// 已在列表里的 ID 直接返回成功。
func Retire(cfg RegistryConfig, instanceID string) error {
	if !instanceIDPattern.MatchString(instanceID) {
		return errors.New("media: 实例 ID 必须是 6 位小写字母或数字")
	}
	if strings.TrimSpace(cfg.ServerAddr) == "" {
		return errors.New("media: 未配置注册中心，无法写入墓碑列表")
	}
	rc, err := newRegistryClients(cfg)
	if err != nil {
		return err
	}
	defer rc.close()
	return retireWith(rc.config, cfg.Group, instanceID, nil)
}

// retireWith 用 CAS 写墓碑列表：读取时记下 md5，写入时带上；失败（别人先写了）则重读、合并、重试，最多 tombstoneCASAttempts 次。
// beforePublish 只用于测试：在第一次写入前执行，用来制造并发写入。
func retireWith(config config_client.IConfigClient, group, instanceID string, beforePublish func()) error {
	var lastErr error
	for attempt := 0; attempt < tombstoneCASAttempts; attempt++ {
		list, casMd5, err := readTombstonesCAS(config, group)
		if err != nil {
			return err
		}
		if tombstoned(list, instanceID) {
			return nil
		}
		content := strings.TrimRight(list, "\n")
		if content != "" {
			content += "\n"
		}
		content += instanceID + "\n"
		if beforePublish != nil {
			beforePublish()
			beforePublish = nil
		}
		ok, err := config.PublishConfig(vo.ConfigParam{DataId: tombstoneDataID, Group: group, Content: content, CasMd5: casMd5})
		if err == nil && ok {
			return nil
		}
		if err == nil {
			err = errors.New("注册中心拒绝写入墓碑列表")
		}
		lastErr = err
	}
	return fmt.Errorf("写入墓碑列表失败（重试 %d 次）: %w", tombstoneCASAttempts, lastErr)
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

// nacosParam 构造 r-nacos 客户端参数。ServerAddr 的 HTTP 端口给 SDK，gRPC 端口由 SDK 按 +1000 推算。
func nacosParam(serverAddr, namespace, username, password string) (vo.NacosClientParam, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(serverAddr))
	if err != nil {
		return vo.NacosClientParam{}, fmt.Errorf("注册中心地址须为 host:port: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return vo.NacosClientParam{}, fmt.Errorf("注册中心端口无效: %w", err)
	}
	return vo.NacosClientParam{
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
	}, nil
}
