package mediasvc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"aigc-local-life/app/media"
)

func testNacos(t *testing.T) string {
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

// fakeInstance 假的媒体服务实例：/v1/cover 返回固定 JPEG，并记下每个实例收到的请求数。
func fakeInstance(t *testing.T, name string, hits map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[name]++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func registryInstances(t *testing.T, addr, service string) int {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/nacos/v1/ns/instance/list?serviceName=%s&healthyOnly=true", addr, service))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Hosts []json.RawMessage `json:"hosts"`
	}
	if json.Unmarshal(body, &out) != nil {
		return 0
	}
	return len(out.Hosts)
}

// 实例通过注册中心加入、注销后离开；调用方实时跟随，故障转移仍然有效。
func TestDiscoveryFollowsRegistrationAndFailsOver(t *testing.T) {
	addr := testNacos(t)
	service := fmt.Sprintf("test-media-disc-%d", time.Now().UnixNano())
	hits := map[string]int{}
	a := fakeInstance(t, "a", hits)
	b := fakeInstance(t, "b", hits)

	// 两个实例各自注册（和媒体服务启动时一样）
	regCtx, regCancel := context.WithCancel(context.Background())
	defer regCancel()
	deregA, err := media.Register(regCtx, media.RegistryConfig{ServerAddr: addr, ServiceName: service, Advertise: a.URL})
	if err != nil {
		t.Fatal(err)
	}
	deregB, err := media.Register(regCtx, media.RegistryConfig{ServerAddr: addr, ServiceName: service, Advertise: b.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer deregB()
	defer deregA()

	c := Discover(DiscoveryConfig{ServerAddr: addr, ServiceName: service}, "tok", nil)
	t.Cleanup(c.Close)
	waitUntil(t, func() bool { return len(c.Instances()) == 2 }, "应当发现两个实例")

	// 同一个视频 URL 总是落到同一个实例
	url := "https://cdn.example/clip-1.mp4"
	for i := 0; i < 5; i++ {
		if _, err := c.Cover(context.Background(), url); err != nil {
			t.Fatal(err)
		}
	}
	first := "a"
	if hits["b"] > 0 {
		first = "b"
	}
	if hits[first] != 5 {
		t.Fatalf("同一视频应当固定到一个实例，实际 hits=%v", hits)
	}

	// 先到的那个实例注销：调用方实时摘掉它，请求全部落到另一个实例
	if first == "a" {
		deregA()
	} else {
		deregB()
	}
	waitUntil(t, func() bool { return len(c.Instances()) == 1 }, "注销的实例应当被摘掉")
	hits = map[string]int{}
	if _, err := c.Cover(context.Background(), url); err != nil {
		t.Fatalf("剩下的实例应当能服务: %v", err)
	}
	if hits[first] != 0 {
		t.Fatalf("已注销的实例不应再收到请求: %v", hits)
	}
}

// 注册中心没有该服务的健康实例时，使用静态回退地址。
func TestDiscoveryFallsBackToStaticWhenNoInstances(t *testing.T) {
	addr := testNacos(t)
	service := fmt.Sprintf("test-media-empty-%d", time.Now().UnixNano())
	hits := map[string]int{}
	fb := fakeInstance(t, "fallback", hits)

	c := Discover(DiscoveryConfig{ServerAddr: addr, ServiceName: service}, "tok", []string{fb.URL})
	t.Cleanup(c.Close)
	if got := c.Instances(); len(got) != 1 || got[0] != fb.URL {
		t.Fatalf("应当回退到静态地址，实际 %v", got)
	}
	if _, err := c.Cover(context.Background(), "https://cdn.example/x.mp4"); err != nil {
		t.Fatal(err)
	}
	if hits["fallback"] != 1 {
		t.Fatalf("回退地址应当收到请求: %v", hits)
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
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
