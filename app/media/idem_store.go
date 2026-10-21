package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 幂等存储（契约 §2）。按 路由 + Idempotency-Key 索引，语义：
//   - 已完成且请求体摘要一致：回放上次的状态码、响应头和响应体（不重复处理，不重复调用供应商）；
//   - 已完成但摘要不同：400 invalid_request（request.idempotency_key_reused）；
//   - 同一个键正在处理中：503 transient + Retry-After，调用方用同一键稍后重试；
//   - 只缓存 2xx。错误不缓存，调用方用同一个键重试会重新执行。
//
// 两种实现：
//   - redis（生产默认）：所有实例共享，路由失效、实例重启、重新路由到另一实例时同一个键仍然只执行一次；
//   - memory：只在本实例内有效，仅用于单机开发。
//
// 存储不可用（Redis 连不上 / 超时）时 begin 返回 errIdemStoreUnavailable，写请求按 503 transient 拒绝，
// 不降级到内存：降级后别的实例看不到这条记录，同一个键会在两台机器上各执行一次（重复调用供应商、重复产生成本）。
type idemStore interface {
	// begin 查询并占用一个键。idemFresh 表示占用成功，调用方必须用返回的 hold 调用 finish（含 panic 路径）。
	begin(ctx context.Context, key string, digest [32]byte) (idemEntry, idemState, *idemHold, error)
	// finish 释放占用；2xx 结果连同摘要写入存储，保留 TTL。非 2xx 只释放，不缓存。
	finish(ctx context.Context, h *idemHold, status int, header http.Header, body []byte) error
	// get 读已完成的结果（不占用、不校验摘要）。供排障与测试使用。
	get(ctx context.Context, key string) (idemEntry, bool, error)
}

// errIdemStoreUnavailable 幂等存储暂时不可用（只有 redis 实现会返回）。
var errIdemStoreUnavailable = errors.New("idempotency store unavailable")

type idemEntry struct {
	digest [32]byte
	status int
	header http.Header
	body   []byte
	at     time.Time
}

type idemState int

const (
	idemFresh idemState = iota
	idemReplay
	idemConflict
	idemBusy
)

// idemHold 一次成功占用的凭据。token 标识占用者：只有占用者能续期、释放、写结果，
// 过期后被别人重新占用的标记不会被旧占用者误删。
type idemHold struct {
	key    string
	digest [32]byte
	token  string
	stop   func() // 停止续期（redis）；memory 为 nil
}

// 幂等存储的配置项。
const (
	IdemStoreRedis  = "redis"
	IdemStoreMemory = "memory"
)

// IdempotencyConfig 幂等存储配置（yaml 键 Idempotency）。
type IdempotencyConfig struct {
	// Store redis | memory。生产默认 redis（多实例共享）；memory 只用于单机开发，启动时打印警告。
	// 留空按 redis（容器里 ${MEDIA_IDEMPOTENCY_STORE} 未设置时展开为空串），取值在 newIdemStoreFromConfig 校验
	Store string `json:",default=redis"`
	// Redis 幂等记录所在的 Redis。Store=redis 时 Addr 必填
	Redis IdemRedisConfig `json:",optional"`
}

// IdemRedisConfig 幂等存储的 Redis 连接。
type IdemRedisConfig struct {
	Addr     string `json:",optional"`
	Username string `json:",optional"`
	Password string `json:",optional"`
	DB       int    `json:",optional"`
	// KeyPrefix 键前缀，与同库其它数据隔离
	KeyPrefix string `json:",default=mediasvc:idem:"`
}

// newIdemStoreFromConfig 按配置创建幂等存储。redis 只校验配置，不在启动时要求连通：
// 启动时 Redis 暂时不可达不阻止服务启动，写请求在恢复前返回 503（见 errIdemStoreUnavailable）。
func newIdemStoreFromConfig(cfg IdempotencyConfig, ttl, lease time.Duration) (idemStore, func(), error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Store)) {
	case IdemStoreMemory:
		log.Printf("media: WARNING 幂等存储为 memory：记录只在本实例内有效，多实例或重启后同一 Idempotency-Key 会被重新执行（可能重复调用供应商）。仅用于单机开发，生产请配置 Idempotency.Store=redis")
		return newMemIdemStore(ttl), func() {}, nil
	case "", IdemStoreRedis:
		if strings.TrimSpace(cfg.Redis.Addr) == "" {
			return nil, nil, errors.New("media: Idempotency.Store=redis（默认）时必须配置 Idempotency.Redis.Addr；单机开发可显式设 Idempotency.Store=memory")
		}
		rdb := redis.NewClient(&redis.Options{
			Addr:         cfg.Redis.Addr,
			Username:     cfg.Redis.Username,
			Password:     cfg.Redis.Password,
			DB:           cfg.Redis.DB,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
		})
		prefix := cfg.Redis.KeyPrefix
		if prefix == "" {
			prefix = "mediasvc:idem:"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Printf("media: 幂等存储 Redis 暂不可达 check=idem.store_unavailable addr=%s: %v（服务照常启动，恢复前写请求返回 503）", cfg.Redis.Addr, err)
		}
		return newRedisIdemStore(rdb, prefix, ttl, lease), func() { _ = rdb.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("media: Idempotency.Store 只能是 redis 或 memory，当前 %q", cfg.Store)
	}
}

// ---------------------------------------------------------------- memory

// memIdemStore 进程内存实现：只在本实例内有效。
type memIdemStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	done     map[string]idemEntry
	inflight map[string][32]byte
}

// sweepAt 已完成的条目超过这个数量时，写入时顺便清理过期的条目。
const sweepAt = 1024

func newMemIdemStore(ttl time.Duration) *memIdemStore {
	return &memIdemStore{ttl: ttl, now: time.Now, done: map[string]idemEntry{}, inflight: map[string][32]byte{}}
}

func (s *memIdemStore) begin(_ context.Context, key string, digest [32]byte) (idemEntry, idemState, *idemHold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.done[key]; ok && s.now().Sub(e.at) < s.ttl {
		if e.digest != digest {
			return idemEntry{}, idemConflict, nil, nil
		}
		return e, idemReplay, nil, nil
	}
	if _, busy := s.inflight[key]; busy {
		return idemEntry{}, idemBusy, nil, nil
	}
	s.inflight[key] = digest
	return idemEntry{}, idemFresh, &idemHold{key: key, digest: digest}, nil
}

func (s *memIdemStore) finish(_ context.Context, h *idemHold, status int, header http.Header, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, h.key)
	if status < 200 || status > 299 {
		return nil
	}
	now := s.now()
	if len(s.done) >= sweepAt {
		for k, e := range s.done {
			if now.Sub(e.at) >= s.ttl {
				delete(s.done, k)
			}
		}
	}
	s.done[h.key] = idemEntry{digest: h.digest, status: status, header: header.Clone(), body: body, at: now}
	return nil
}

func (s *memIdemStore) get(_ context.Context, key string) (idemEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.done[key]
	if !ok || s.now().Sub(e.at) >= s.ttl {
		return idemEntry{}, false, nil
	}
	return e, true, nil
}

// ---------------------------------------------------------------- redis

// redisIdemStore Redis 实现，所有实例共享。每个键两条记录：
//
//	{prefix}lock:{h}  处理中标记，值为占用者 token，SET NX PX lease；处理期间每 lease/3 续期一次
//	{prefix}done:{h}  完成结果（摘要、状态码、响应头、响应体），PX ttl（默认 120 分钟）
//
// h 是 路由|Idempotency-Key 的 SHA-256：调用方给的键长度不受控，不直接拼进 Redis 键。
//
// 处理中标记用短租约 + 续期，而不是覆盖整个处理时长的长 TTL：实例崩溃后标记最多 lease 时长就自动释放，
// 同一个键不会被卡上几十分钟；正常处理多久都不会过期。
type redisIdemStore struct {
	rdb    redis.UniversalClient
	prefix string
	ttl    time.Duration
	lease  time.Duration
	// opTimeout 单次 Redis 操作的时限：存储不可用时尽快返回 503，不把请求挂住
	opTimeout time.Duration
}

func newRedisIdemStore(rdb redis.UniversalClient, prefix string, ttl, lease time.Duration) *redisIdemStore {
	if lease <= 0 {
		lease = defaultIdemLease
	}
	return &redisIdemStore{rdb: rdb, prefix: prefix, ttl: ttl, lease: lease, opTimeout: 3 * time.Second}
}

// defaultIdemLease 处理中标记的租约。崩溃后最多这么久同一个键才能重新执行。
const defaultIdemLease = 60 * time.Second

// redisDone 完成结果的存储格式。Body 由 encoding/json 以 base64 编码。
type redisDone struct {
	Digest string      `json:"d"`
	Status int         `json:"s"`
	Header http.Header `json:"h,omitempty"`
	Body   []byte      `json:"b"`
	At     int64       `json:"t"`
}

// beginScript 原子地：有完成结果就返回它；否则尝试占用处理中标记。
// 返回 {1, done} 已完成；{2} 正在处理；{0} 占用成功。
var beginScript = redis.NewScript(`
local done = redis.call('GET', KEYS[1])
if done then return {1, done} end
if redis.call('SET', KEYS[2], ARGV[1], 'NX', 'PX', ARGV[2]) then return {0} end
return {2}
`)

// renewScript 只续自己的租约。
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 0
`)

// releaseScript 只删自己的标记。
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

// finishScript 写完成结果（已有结果时保留先写入的，回放结果稳定）并删自己的标记。
// 结果与释放在同一个脚本里：别的实例不会看到"标记已删但结果未写"的窗口而重新执行。
var finishScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[2], 'NX', 'PX', ARGV[3])
if redis.call('GET', KEYS[2]) == ARGV[1] then redis.call('DEL', KEYS[2]) end
return 1
`)

func (s *redisIdemStore) keys(key string) (done, lock string) {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return s.prefix + "done:" + h, s.prefix + "lock:" + h
}

// opCtx Redis 操作用的上下文：脱离请求的取消（请求超时后仍要释放标记、写结果），只受 opTimeout 约束。
func (s *redisIdemStore) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.opTimeout)
}

func (s *redisIdemStore) begin(ctx context.Context, key string, digest [32]byte) (idemEntry, idemState, *idemHold, error) {
	doneKey, lockKey := s.keys(key)
	token, err := newIdemToken()
	if err != nil {
		return idemEntry{}, 0, nil, fmt.Errorf("%w: %v", errIdemStoreUnavailable, err)
	}
	c, cancel := s.opCtx(ctx)
	defer cancel()
	res, err := beginScript.Run(c, s.rdb, []string{doneKey, lockKey}, token, s.lease.Milliseconds()).Slice()
	if err != nil {
		return idemEntry{}, 0, nil, fmt.Errorf("%w: begin: %v", errIdemStoreUnavailable, err)
	}
	if len(res) == 0 {
		return idemEntry{}, 0, nil, fmt.Errorf("%w: begin: 空结果", errIdemStoreUnavailable)
	}
	code, _ := res[0].(int64)
	switch code {
	case 0:
		h := &idemHold{key: key, digest: digest, token: token}
		h.stop = s.keepAlive(ctx, lockKey, token)
		return idemEntry{}, idemFresh, h, nil
	case 1:
		raw, _ := res[1].(string)
		e, err := decodeDone(raw)
		if err != nil {
			// 存储内容损坏：既不能回放，也不能放行重算（可能重复调用供应商），按存储不可用处理
			return idemEntry{}, 0, nil, fmt.Errorf("%w: 结果无法解析: %v", errIdemStoreUnavailable, err)
		}
		if e.digest != digest {
			return idemEntry{}, idemConflict, nil, nil
		}
		return e, idemReplay, nil, nil
	default:
		return idemEntry{}, idemBusy, nil, nil
	}
}

// keepAlive 处理期间每 lease/3 续一次租约，直到 stop。续期失败只记日志：
// 标记过期后同一个键可能被另一实例重新执行，所以续期必须比租约短得多（留两次重试的余量）。
func (s *redisIdemStore) keepAlive(ctx context.Context, lockKey, token string) func() {
	stopCh := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(s.lease / 3)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				c, cancel := s.opCtx(ctx)
				n, err := renewScript.Run(c, s.rdb, []string{lockKey}, token, s.lease.Milliseconds()).Int64()
				cancel()
				if err != nil {
					log.Printf("media: 幂等标记续期失败 check=idem.store_unavailable: %v", err)
				} else if n == 0 {
					log.Printf("media: 幂等标记已不属于本次处理 check=idem.lease_lost（租约过期，同一键可能被重新执行）")
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(stopCh) }) }
}

func (s *redisIdemStore) finish(ctx context.Context, h *idemHold, status int, header http.Header, body []byte) error {
	if h.stop != nil {
		h.stop()
	}
	doneKey, lockKey := s.keys(h.key)
	c, cancel := s.opCtx(ctx)
	defer cancel()
	if status < 200 || status > 299 || s.ttl <= 0 {
		if err := releaseScript.Run(c, s.rdb, []string{lockKey}, h.token).Err(); err != nil {
			return fmt.Errorf("%w: release: %v", errIdemStoreUnavailable, err)
		}
		return nil
	}
	raw, err := json.Marshal(redisDone{Digest: hex.EncodeToString(h.digest[:]), Status: status, Header: header, Body: body, At: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	if err := finishScript.Run(c, s.rdb, []string{doneKey, lockKey}, h.token, raw, s.ttl.Milliseconds()).Err(); err != nil {
		return fmt.Errorf("%w: finish: %v", errIdemStoreUnavailable, err)
	}
	return nil
}

func (s *redisIdemStore) get(ctx context.Context, key string) (idemEntry, bool, error) {
	doneKey, _ := s.keys(key)
	c, cancel := s.opCtx(ctx)
	defer cancel()
	raw, err := s.rdb.Get(c, doneKey).Result()
	if errors.Is(err, redis.Nil) {
		return idemEntry{}, false, nil
	}
	if err != nil {
		return idemEntry{}, false, fmt.Errorf("%w: get: %v", errIdemStoreUnavailable, err)
	}
	e, err := decodeDone(raw)
	if err != nil {
		return idemEntry{}, false, fmt.Errorf("%w: 结果无法解析: %v", errIdemStoreUnavailable, err)
	}
	return e, true, nil
}

func decodeDone(raw string) (idemEntry, error) {
	var d redisDone
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return idemEntry{}, err
	}
	sum, err := hex.DecodeString(d.Digest)
	if err != nil || len(sum) != sha256.Size {
		return idemEntry{}, errors.New("摘要格式错误")
	}
	e := idemEntry{status: d.Status, header: d.Header, body: d.Body, at: time.UnixMilli(d.At)}
	copy(e.digest[:], sum)
	return e, nil
}

func newIdemToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
