package media

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 两台机器 = 两个独立的 Server（各自的 Redis 客户端、各自的 handler），共享同一个 Redis。
// handler 计数记在 backendCalls 上（模拟"调用供应商"），断言只执行一次。

func newRedisIdemServer(t *testing.T, addr, prefix string, lease time.Duration) *Server {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { _ = rdb.Close() })
	return &Server{idem: newRedisIdemStore(rdb, prefix, 120*time.Minute, lease)}
}

func countingHandler(s *Server, backendCalls *atomic.Int32, release <-chan struct{}) http.HandlerFunc {
	return s.idempotent("test", func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		if release != nil {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Resource-Id", "rid-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"resource_id":"rid-1"}`))
	})
}

// 实例 A 执行，实例 B 用同一个键：命中 A 的结果（状态码、头、体一致），后端只执行一次。
func TestRedisIdemSharedAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	var calls atomic.Int32
	a := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)
	b := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)

	first := idemRequest(a, "k-shared", `{"a":1}`)
	second := idemRequest(b, "k-shared", `{"a":1}`)
	if calls.Load() != 1 {
		t.Fatalf("后端执行次数 = %d，want 1（第二台机器应回放第一台的结果）", calls.Load())
	}
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || second.Body.String() != first.Body.String() {
		t.Fatalf("回放不一致: first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" || second.Header().Get("X-Resource-Id") != "rid-1" ||
		second.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("回放应带上原响应头与 Idempotent-Replayed: %v", second.Header())
	}
	// 记录 TTL 为 120 分钟
	keys := mr.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "t:idem:done:") {
		t.Fatalf("完成后应只剩结果记录（处理中标记已删）: %v", keys)
	}
	if ttl := mr.TTL(keys[0]); ttl != 120*time.Minute {
		t.Fatalf("结果 TTL = %v, want 120m", ttl)
	}
}

// 实例 A 正在处理时，实例 B 同一个键：503 transient + Retry-After，不执行。
func TestRedisIdemInFlightOnOtherInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	var calls atomic.Int32
	release := make(chan struct{})
	a := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, release)
	b := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); idemRequest(a, "k-busy", `{"a":1}`) }()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rec := idemRequest(b, "k-busy", `{"a":1}`)
	close(release)
	wg.Wait()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" ||
		!strings.Contains(rec.Body.String(), `"check":"request.idempotency_in_flight"`) {
		t.Fatalf("处理中应 503 in_flight: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("后端执行次数 = %d，want 1", calls.Load())
	}
}

// 同一个键、不同请求体（跨实例）：400 invalid_request，不执行。
func TestRedisIdemConflictAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	var calls atomic.Int32
	a := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)
	b := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)
	idemRequest(a, "k-conflict", `{"a":1}`)
	rec := idemRequest(b, "k-conflict", `{"a":2}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "request.idempotency_key_reused") {
		t.Fatalf("同键异体应 400: %d %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("后端执行次数 = %d，want 1", calls.Load())
	}
}

// Redis 不可用：写请求 503 transient（check=idem.store_unavailable），不执行（不降级到内存重算）。
func TestRedisIdemStoreUnavailableReturns503(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 端口已关闭：连接被拒

	var calls atomic.Int32
	h := countingHandler(newRedisIdemServer(t, addr, "t:idem:", time.Minute), &calls, nil)
	rec := idemRequest(h, "k-down", `{"a":1}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), `"check":"idem.store_unavailable"`) ||
		!strings.Contains(rec.Body.String(), `"code":"transient"`) {
		t.Fatalf("存储不可用应 503 transient idem.store_unavailable: %d %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("存储不可用时不能执行 handler（会在别的实例上重复调用供应商），calls = %d", calls.Load())
	}
}

// Redis 中途宕机后恢复：宕机期间 503，恢复后同一个键正常执行一次。
func TestRedisIdemRecoversAfterOutage(t *testing.T) {
	mr := miniredis.RunT(t)
	var calls atomic.Int32
	h := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)
	mr.SetError("LOADING simulated outage")
	if rec := idemRequest(h, "k-flap", `{"a":1}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("宕机期间应 503，得到 %d", rec.Code)
	}
	mr.SetError("")
	if rec := idemRequest(h, "k-flap", `{"a":1}`); rec.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("恢复后应执行一次: %d calls=%d", rec.Code, calls.Load())
	}
}

// handler panic：500 可重试，处理中标记释放（另一台机器同键立即可执行，而不是 503 in_flight），失败不缓存。
func TestRedisIdemPanicReleasesMark(t *testing.T) {
	mr := miniredis.RunT(t)
	s := newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute)
	boom := s.idempotent("test", func(http.ResponseWriter, *http.Request) { panic("boom") })
	first := idemRequest(boom, "k-panic", `{"a":1}`)
	if first.Code != http.StatusInternalServerError || !strings.Contains(first.Body.String(), `"retryable":true`) {
		t.Fatalf("panic 应 500 可重试: %d %s", first.Code, first.Body.String())
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("panic 后不应留下标记或结果: %v", keys)
	}
	var calls atomic.Int32
	other := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)
	if rec := idemRequest(other, "k-panic", `{"a":1}`); rec.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("panic 后同键应可重新执行: %d calls=%d", rec.Code, calls.Load())
	}
}

// 错误结果不缓存（跨实例）：A 失败，B 同键重新执行。
func TestRedisIdemDoesNotCacheErrors(t *testing.T) {
	mr := miniredis.RunT(t)
	var calls atomic.Int32
	failing := newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute)
	f := failing.idempotent("test", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeAPIError(w, newAPIError(codeTransient, "test.fail"))
	})
	if rec := idemRequest(f, "k-err", `{"a":1}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first = %d", rec.Code)
	}
	ok := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", time.Minute), &calls, nil)
	if rec := idemRequest(ok, "k-err", `{"a":1}`); rec.Code != http.StatusCreated || calls.Load() != 2 {
		t.Fatalf("错误不缓存，同键应重新执行: %d calls=%d", rec.Code, calls.Load())
	}
}

// 处理时长超过租约：续期保持标记，另一实例仍然看到 in_flight，不会并发重算。
func TestRedisIdemLeaseRenewedDuringLongProcessing(t *testing.T) {
	mr := miniredis.RunT(t)
	lease := 300 * time.Millisecond
	var calls atomic.Int32
	release := make(chan struct{})
	a := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", lease), &calls, release)
	b := countingHandler(newRedisIdemServer(t, mr.Addr(), "t:idem:", lease), &calls, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); idemRequest(a, "k-long", `{"a":1}`) }()
	for calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// miniredis 的过期靠 FastForward 推进：分多步推进到超过租约数倍，期间真实时间里续期持续发生
	for i := 0; i < 12; i++ { // 共推进 2 倍租约；没有续期时标记早已过期
		time.Sleep(lease / 3)
		mr.FastForward(lease / 6)
	}
	rec := idemRequest(b, "k-long", `{"a":1}`)
	close(release)
	wg.Wait()
	if rec.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("长处理期间标记应被续期: %d calls=%d", rec.Code, calls.Load())
	}
}

// 字节上传入口（uploadBytes）走同一个存储：Redis 不可用时 503，不落桶。
func TestUploadBytesIdemStoreUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), IdempotencyMinutes: 10,
		Idempotency: IdempotencyConfig{Store: IdemStoreRedis, Redis: IdemRedisConfig{Addr: addr}}})
	resp := uploadBytes(t, ts.URL, "intermediate", "k-up-down", []byte("anything"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("存储不可用时字节上传应 503，得到 %d", resp.StatusCode)
	}
}

// 配置校验：redis 必须给地址；未知取值拒绝；memory 可用。
func TestIdemStoreConfig(t *testing.T) {
	if _, _, err := newIdemStoreFromConfig(IdempotencyConfig{}, time.Minute, time.Second); err == nil {
		t.Fatal("默认 redis 且未配地址应拒绝启动")
	}
	if _, _, err := newIdemStoreFromConfig(IdempotencyConfig{Store: "file"}, time.Minute, time.Second); err == nil {
		t.Fatal("未知的 Store 应拒绝启动")
	}
	s, closeFn, err := newIdemStoreFromConfig(IdempotencyConfig{Store: IdemStoreMemory}, time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	closeFn()
	if _, ok := s.(*memIdemStore); !ok {
		t.Fatalf("got %T", s)
	}
}

// 真实 Redis 集成验证（可选）：MEDIA_IDEM_TEST_REDIS=host:port 时运行，使用临时键前缀，结束后删除。
func TestRedisIdemRealRedisIntegration(t *testing.T) {
	addr := os.Getenv("MEDIA_IDEM_TEST_REDIS")
	if addr == "" {
		t.Skip("未设置 MEDIA_IDEM_TEST_REDIS，跳过真实 Redis 集成验证")
	}
	prefix := "mediasvc:idemtest:" + newTraceID() + ":"
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("MEDIA_IDEM_TEST_REDIS_PASSWORD")})
	t.Cleanup(func() { _ = rdb.Close() }) // 先注册后执行：删键在关闭之前
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	mk := func() *Server {
		c := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("MEDIA_IDEM_TEST_REDIS_PASSWORD")})
		t.Cleanup(func() { _ = c.Close() })
		return &Server{idem: newRedisIdemStore(c, prefix, 120*time.Minute, time.Minute)}
	}
	var calls atomic.Int32
	a, b := countingHandler(mk(), &calls, nil), countingHandler(mk(), &calls, nil)
	first := idemRequest(a, "k-real", `{"a":1}`)
	second := idemRequest(b, "k-real", `{"a":1}`)
	if calls.Load() != 1 || second.Body.String() != first.Body.String() || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("真实 Redis 跨实例回放失败: calls=%d first=%s second=%s", calls.Load(), first.Body.String(), second.Body.String())
	}
	if rec := idemRequest(b, "k-real", `{"a":2}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("同键异体应 400: %d", rec.Code)
	}
	keys, err := rdb.Keys(context.Background(), prefix+"*").Result()
	if err != nil || len(keys) != 1 {
		t.Fatalf("应只剩一条结果记录: %v %v", keys, err)
	}
	if ttl := rdb.PTTL(context.Background(), keys[0]).Val(); ttl <= 119*time.Minute || ttl > 120*time.Minute {
		t.Fatalf("结果 TTL = %v", ttl)
	}
}
