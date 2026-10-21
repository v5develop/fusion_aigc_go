package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/model"
)

// nacosAddr 测试用注册中心地址：MEDIA_TEST_NACOS 指定，默认本机 8848。连不上就跳过。
func nacosAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("MEDIA_TEST_NACOS")
	if addr == "" {
		addr = "127.0.0.1:8848"
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/nacos/v1/ns/service/list?pageNo=1&pageSize=1")
	if err != nil {
		t.Skipf("注册中心不可用: %v", err)
	}
	resp.Body.Close()
	return addr
}

// instanceCount 通过 OpenAPI 查注册中心里某个服务的实例数（不管健康与否）。
func instanceCount(t *testing.T, addr, service string) int {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/nacos/v1/ns/instance/list?serviceName=%s&healthyOnly=false", addr, service))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Hosts []json.RawMessage `json:"hosts"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
		return 0 // 服务不存在时返回 404 或空结果，都按 0 个实例处理
	}
	return len(out.Hosts)
}

// 注册后实例出现在注册中心；注销后立即消失。
func TestRegisterThenDeregisterVisibleInRegistry(t *testing.T) {
	addr := nacosAddr(t)
	service := fmt.Sprintf("test-media-reg-%d", time.Now().UnixNano())

	deregister, err := Register(context.Background(), RegistryConfig{
		ServerAddr: addr, ServiceName: service, Advertise: "http://127.0.0.1:18990",
	}, "tst001", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return instanceCount(t, addr, service) == 1 }, "实例应当出现在注册中心")

	deregister()
	waitFor(t, func() bool { return instanceCount(t, addr, service) == 0 }, "注销后实例应当消失")
}

// 注册中心只接受 IP：Advertise 用域名直接报错，不能注册一个别人解析不了的地址。
func TestAdvertiseMustBeIP(t *testing.T) {
	if _, err := instanceOf("http://media.internal:18090"); err == nil {
		t.Fatal("域名地址必须被拒绝")
	}
	inst, err := instanceOf("https://10.0.0.5:18090")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ip != "10.0.0.5" || inst.port != 18090 || inst.scheme != "https" {
		t.Fatalf("解析结果 = %+v", inst)
	}
}

// 没配注册中心：不注册，也不报错。
func TestRegisterWithoutServerIsNoop(t *testing.T) {
	deregister, err := Register(context.Background(), RegistryConfig{}, "tst001", nil)
	if err != nil {
		t.Fatal(err)
	}
	deregister()
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal(msg)
}

// 同一个实例 ID 只能在一个地址上存活：同 ID 的存活实例在别的地址上，注册被拒绝。
func TestDuplicateInstanceIDIsRefused(t *testing.T) {
	addr := nacosAddr(t)
	service := fmt.Sprintf("test-media-dup-%d", time.Now().UnixNano())
	id := uniqueInstanceID()
	first, err := Register(context.Background(), RegistryConfig{
		ServerAddr: addr, ServiceName: service, Advertise: "http://127.0.0.1:18992",
	}, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	waitFor(t, func() bool { return instanceCount(t, addr, service) == 1 }, "第一个实例应当出现在注册中心")

	_, err = Register(context.Background(), RegistryConfig{
		ServerAddr: addr, ServiceName: service, Advertise: "http://127.0.0.1:18993",
	}, id, nil)
	if !errors.Is(err, ErrInstanceIDInUse) {
		t.Fatalf("同 ID 的第二个地址应被拒绝，got %v", err)
	}
}

// 墓碑：下线后的 ID 永不能再注册；重复下线是幂等的。
func TestRetiredInstanceIDIsRefused(t *testing.T) {
	addr := nacosAddr(t)
	cfg := RegistryConfig{ServerAddr: addr, ServiceName: fmt.Sprintf("test-media-retire-%d", time.Now().UnixNano()), Advertise: "http://127.0.0.1:18994"}
	id := uniqueInstanceID()
	if err := Retire(cfg, id); err != nil {
		t.Fatal(err)
	}
	if err := Retire(cfg, id); err != nil {
		t.Fatalf("重复下线应当幂等: %v", err)
	}
	_, err := Register(context.Background(), cfg, id, nil)
	if !errors.Is(err, ErrInstanceIDRetired) {
		t.Fatalf("已下线的 ID 应被拒绝，got %v", err)
	}
}

func uniqueInstanceID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	n := time.Now().UnixNano()
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[n%int64(len(alphabet))]
		n /= int64(len(alphabet))
	}
	return string(b)
}

// 纯函数：同 ID 同地址（本实例重启）允许，同 ID 不同地址拒绝，不同 ID 不影响。
func TestCheckInstanceID(t *testing.T) {
	self := instance{ip: "10.0.0.5", port: 18090, scheme: "http"}
	live := []model.Instance{
		{Ip: "10.0.0.5", Port: 18090, Enable: true, Metadata: map[string]string{instanceIDMetaKey: "abc123"}},
		{Ip: "10.0.0.9", Port: 18090, Enable: true, Metadata: map[string]string{instanceIDMetaKey: "zzz999"}},
	}
	if err := checkInstanceID(live, "abc123", self); err != nil {
		t.Fatalf("同地址重启应允许: %v", err)
	}
	if err := checkInstanceID(live, "zzz999", self); !errors.Is(err, ErrInstanceIDInUse) {
		t.Fatalf("别的地址占用应拒绝: %v", err)
	}
	if err := checkInstanceID(live, "new001", self); err != nil {
		t.Fatalf("未占用的 ID 应允许: %v", err)
	}
	if !IsRefusedRegistration(ErrInstanceIDInUse) || IsRefusedRegistration(errors.New("网络")) {
		t.Fatal("只有 ID 冲突与已下线才算拒绝注册")
	}
}

// 纯函数：墓碑列表按行匹配，忽略空白与注释。
func TestTombstoned(t *testing.T) {
	list := "# 已下线\nold001\n  old002  \n"
	if !tombstoned(list, "old001") || !tombstoned(list, "old002") {
		t.Fatal("列表里的 ID 应命中")
	}
	if tombstoned(list, "# 已下线") || tombstoned(list, "old00") || tombstoned("", "old001") {
		t.Fatal("注释、前缀与空列表不应命中")
	}
}
