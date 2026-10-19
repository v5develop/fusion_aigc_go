package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
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
	})
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
	deregister, err := Register(context.Background(), RegistryConfig{})
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
