package media

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

const testRegistryGroup = "DEFAULT_GROUP"

// 真实注册中心：测试用的实例 ID 必须唯一，避免和其它测试或历史数据撞上。
func uniqueTestInstanceID(prefix string) string {
	return fmt.Sprintf("%s%05d", prefix, time.Now().UnixNano()%100000)
}

func testRegistryConfig(addr, service string) RegistryConfig {
	return RegistryConfig{ServerAddr: addr, ServiceName: service, Group: testRegistryGroup, Advertise: "http://10.0.0.5:18990"}
}

// casConfigClient 模拟 Nacos 的 CAS 语义：casMd5 非空且与当前内容不符时拒绝写入（返回 false，不报错）。
// r-nacos v0.8.7 不执行 casMd5（已实测：错误的 md5 也被接受），所以用包装器验证调用方的冲突处理逻辑；
// 底层仍是真实的 SDK 客户端。
type casConfigClient struct {
	config_client.IConfigClient
	rejected int
}

func (c *casConfigClient) PublishConfig(param vo.ConfigParam) (bool, error) {
	if param.CasMd5 != "" {
		cur, err := c.IConfigClient.GetConfig(vo.ConfigParam{DataId: param.DataId, Group: param.Group})
		if err == nil {
			sum := md5.Sum([]byte(cur))
			if hex.EncodeToString(sum[:]) != param.CasMd5 {
				c.rejected++
				return false, nil
			}
		}
	}
	return c.IConfigClient.PublishConfig(param)
}

// alwaysRejectClient 每次写入都被 CAS 拒绝。
type alwaysRejectClient struct {
	config_client.IConfigClient
	attempts int
}

func (c *alwaysRejectClient) PublishConfig(vo.ConfigParam) (bool, error) {
	c.attempts++
	return false, nil
}

// 墓碑写入的 CAS：读取之后、写入之前有别人追加了另一个 ID，我们的写入必须被拒绝、重读并合并，两个 ID 都要保留。
// 没有 CAS 时，后写的会覆盖掉先写的，被覆盖的 ID 就不再是墓碑，可以被复用。
func TestRetireCASKeepsConcurrentTombstone(t *testing.T) {
	addr := nacosAddr(t)
	cfg := testRegistryConfig(addr, "test-media-cas")
	rc, err := newRegistryClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	// 配置项必须已存在，否则 CAS 不生效（不存在时 md5 为空，不做比较）
	seed := uniqueTestInstanceID("cas")
	if ok, err := rc.config.PublishConfig(vo.ConfigParam{DataId: tombstoneDataID, Group: cfg.Group, Content: seed + "\n"}); err != nil || !ok {
		t.Fatalf("准备墓碑配置失败: ok=%v err=%v", ok, err)
	}

	cas := &casConfigClient{IConfigClient: rc.config}
	idA := uniqueTestInstanceID("cax")
	idB := uniqueTestInstanceID("cbx")
	err = retireWith(cas, cfg.Group, idA, func() {
		// 读取之后、写入之前：另一个写入者追加了 idB（不走 CAS）
		list, err := readTombstones(rc.config, cfg.Group)
		if err != nil {
			t.Fatal(err)
		}
		content := strings.TrimRight(list, "\n") + "\n" + idB + "\n"
		if ok, err := rc.config.PublishConfig(vo.ConfigParam{DataId: tombstoneDataID, Group: cfg.Group, Content: content}); err != nil || !ok {
			t.Fatalf("模拟并发写入失败: ok=%v err=%v", ok, err)
		}
	})
	if err != nil {
		t.Fatalf("CAS 冲突后应重读重试成功: %v", err)
	}
	if cas.rejected != 1 {
		t.Fatalf("第一次写入应因 CAS 被拒绝一次，实际 %d 次", cas.rejected)
	}
	// SDK 在 CAS 写入之后的一小段时间里可能还读到旧缓存（已实测约 2 秒内），所以轮询到一致为止
	var final string
	deadline := time.Now().Add(5 * time.Second)
	for {
		final, err = readTombstones(rc.config, cfg.Group)
		if err != nil {
			t.Fatal(err)
		}
		if tombstoned(final, idA) && tombstoned(final, idB) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("两个墓碑都应保留，得到: %q", final)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// CAS 持续失败：重试 tombstoneCASAttempts 次后返回错误，不无限重试。
func TestRetireGivesUpAfterCASAttempts(t *testing.T) {
	addr := nacosAddr(t)
	cfg := testRegistryConfig(addr, "test-media-cas-giveup")
	rc, err := newRegistryClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	if ok, err := rc.config.PublishConfig(vo.ConfigParam{DataId: tombstoneDataID, Group: cfg.Group, Content: "# seed\n"}); err != nil || !ok {
		t.Fatalf("准备墓碑配置失败: ok=%v err=%v", ok, err)
	}
	always := &alwaysRejectClient{IConfigClient: rc.config}
	err = retireWith(always, cfg.Group, uniqueTestInstanceID("gvu"), nil)
	if err == nil {
		t.Fatal("CAS 持续失败应返回错误")
	}
	if always.attempts != tombstoneCASAttempts {
		t.Fatalf("尝试次数 = %d, want %d", always.attempts, tombstoneCASAttempts)
	}
}

// 注册前检查：别的地址上已有同 ID 的健康实例，必须拒绝（只看不健康实例的查询会漏掉它）。
func TestRegisterRefusesIDHeldByHealthyInstance(t *testing.T) {
	addr := nacosAddr(t)
	service := fmt.Sprintf("test-media-held-%d", time.Now().UnixNano())
	cfg := testRegistryConfig(addr, service)
	id := uniqueTestInstanceID("hld")

	rc, err := newRegistryClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	ok, err := rc.naming.RegisterInstance(vo.RegisterInstanceParam{
		Ip: "10.0.0.9", Port: 18990, Weight: 1, Enable: true, Healthy: true, Ephemeral: true,
		ServiceName: service, GroupName: cfg.Group, Metadata: map[string]string{instanceIDMetaKey: id},
	})
	if err != nil || !ok {
		t.Fatalf("准备占用方实例失败: ok=%v err=%v", ok, err)
	}
	defer rc.naming.DeregisterInstance(vo.DeregisterInstanceParam{
		Ip: "10.0.0.9", Port: 18990, ServiceName: service, GroupName: cfg.Group, Ephemeral: true,
	})

	_, err = Register(context.Background(), cfg, id, nil)
	if !errors.Is(err, ErrInstanceIDInUse) {
		t.Fatalf("健康的同 ID 实例应拒绝注册，得到 err=%v", err)
	}
}

// 禁用（enable=false）的实例不占 ID：管理员下线的实例不应阻止复用。
func TestCheckInstanceIDIgnoresDisabledInstance(t *testing.T) {
	self := instance{ip: "10.0.0.5", port: 18990, scheme: "http"}
	registered := []model.Instance{{
		Ip: "10.0.0.9", Port: 18990, Enable: false, Metadata: map[string]string{instanceIDMetaKey: "dis001"},
	}}
	if err := checkInstanceID(registered, "dis001", self); err != nil {
		t.Fatalf("禁用的实例不应占用 ID: %v", err)
	}
	registered[0].Enable = true
	if err := checkInstanceID(registered, "dis001", self); err == nil {
		t.Fatal("启用的别的地址实例应占用 ID")
	}
}

// 注册成功后周期复查：之后别的地址抢占同一个 ID，必须通过 onFatal 通知。
func TestRegisterRechecksIDAfterRegistration(t *testing.T) {
	addr := nacosAddr(t)
	service := fmt.Sprintf("test-media-recheck-%d", time.Now().UnixNano())
	cfg := testRegistryConfig(addr, service)
	id := uniqueTestInstanceID("rck")

	old := instanceRecheckInterval
	instanceRecheckInterval = 100 * time.Millisecond
	t.Cleanup(func() { instanceRecheckInterval = old })

	fatal := make(chan error, 1)
	deregister, err := Register(context.Background(), cfg, id, func(err error) {
		select {
		case fatal <- err:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deregister()

	// 注册成功后，别的地址抢到同一个 ID
	rc, err := newRegistryClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	ok, err := rc.naming.RegisterInstance(vo.RegisterInstanceParam{
		Ip: "10.0.0.8", Port: 18990, Weight: 1, Enable: true, Healthy: true, Ephemeral: true,
		ServiceName: service, GroupName: cfg.Group, Metadata: map[string]string{instanceIDMetaKey: id},
	})
	if err != nil || !ok {
		t.Fatalf("准备冲突实例失败: ok=%v err=%v", ok, err)
	}
	defer rc.naming.DeregisterInstance(vo.DeregisterInstanceParam{
		Ip: "10.0.0.8", Port: 18990, ServiceName: service, GroupName: cfg.Group, Ephemeral: true,
	})

	select {
	case err := <-fatal:
		if !IsRefusedRegistration(err) {
			t.Fatalf("onFatal 应收到 ID 冲突错误，得到 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("注册后出现 ID 冲突，复查应通过 onFatal 通知")
	}
}
