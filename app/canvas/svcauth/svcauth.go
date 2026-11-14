// Package svcauth 服务间 HMAC 鉴权（Java 智能体 → Go 画布内部入口 /internal/v1/canvas/agent/*）。
//
// 请求头：
//
//	X-Svc-Key-Id     密钥 id（服务端配置多把，按 id 选 secret，支持轮换）
//	X-Svc-Timestamp  unix 秒（十进制）；与服务端时间相差超过 ±300 秒拒绝
//	X-Svc-Nonce      16–64 位 [A-Za-z0-9_-]；同一 key 下 600 秒内只能用一次（Redis SETNX）
//	X-Svc-User-Id    代哪个用户操作（十进制正整数；GET /tools 可为 0）
//	X-Svc-Signature  hex(HMAC-SHA256(secret, stringToSign))，小写
//
// stringToSign = METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + NONCE + "\n" + USER_ID + "\n" + hex(sha256(body))
//
// METHOD 大写；PATH 是网关收到的 URL 路径（不含 query 与域名，如 /internal/v1/canvas/agent/call）；body 为空时取空串的 sha256。
// 校验顺序：头齐全 / 格式 → key 存在 → 时间窗 → 签名 → nonce 占用（签名通过后才占 nonce，伪造请求烧不掉别人的 nonce）。
// 契约文档：docs/design/canvas-agent-tools.md §12。
package svcauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 请求头。
const (
	HeaderKeyID     = "X-Svc-Key-Id"
	HeaderTimestamp = "X-Svc-Timestamp"
	HeaderNonce     = "X-Svc-Nonce"
	HeaderUserID    = "X-Svc-User-Id"
	HeaderSignature = "X-Svc-Signature"
)

// 拒绝原因（reason，随 2001 返回；HTTP 401）。
const (
	ReasonMissing      = "SVC_AUTH_MISSING"       // 缺头 / 头格式不对
	ReasonUserMissing  = "SVC_USER_MISSING"       // 缺 X-Svc-User-Id 或不是正整数
	ReasonUnknownKey   = "SVC_KEY_UNKNOWN"        // key id 不在配置里
	ReasonSkew         = "SVC_TIMESTAMP_EXPIRED"  // 时间戳超出 ±窗口
	ReasonBadSignature = "SVC_SIGNATURE_INVALID"  // 签名不对
	ReasonReplay       = "SVC_NONCE_REPLAYED"     // nonce 已用过
	ReasonNonceStore   = "SVC_NONCE_STORE_FAILED" // nonce 存储不可用（服务端问题，可重试；5001）
)

// nonceGrace nonce 保留时长在 2×时间窗之外的余量（时钟抖动、Redis 过期精度）。
const nonceGrace = 30 * time.Second

// MinSecretLen secret 最短长度（字节）；更短的 key 不加载。
const MinSecretLen = 32

var (
	keyIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,48}$`)
	nonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
)

// StringToSign 待签名串。
func StringToSign(method, path, timestamp, nonce, userID string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.ToUpper(method) + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + userID + "\n" + hex.EncodeToString(sum[:])
}

// Sign hex(HMAC-SHA256(secret, stringToSign))。
func Sign(secret, method, path, timestamp, nonce, userID string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(StringToSign(method, path, timestamp, nonce, userID, body)))
	return hex.EncodeToString(m.Sum(nil))
}

// SignRequest 给请求加签（调用方 / 测试用）：设置五个头。
func SignRequest(r *http.Request, keyID, secret string, userID int64, nonce string, now time.Time, body []byte) {
	ts := strconv.FormatInt(now.Unix(), 10)
	uid := strconv.FormatInt(userID, 10)
	r.Header.Set(HeaderKeyID, keyID)
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderUserID, uid)
	r.Header.Set(HeaderSignature, Sign(secret, r.Method, r.URL.Path, ts, nonce, uid, body))
}

// NonceStore nonce 防重放：首次占用返回 true。
type NonceStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// RedisNonces Redis SETNX（多实例共享）。
type RedisNonces struct{ R redis.UniversalClient }

func (s RedisNonces) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return s.R.SetNX(ctx, key, "1", ttl).Result()
}

// MemoryNonces 进程内（仅开发 / 单实例；多实例时各算各的）。
type MemoryNonces struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func NewMemoryNonces() *MemoryNonces {
	return &MemoryNonces{seen: map[string]time.Time{}, now: time.Now}
}

func (s *MemoryNonces) Claim(_ context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.seen) > 100000 { // 粗暴清理过期项，防止无限增长
		for k, exp := range s.seen {
			if now.After(exp) {
				delete(s.seen, k)
			}
		}
	}
	if exp, ok := s.seen[key]; ok && now.Before(exp) {
		return false, nil
	}
	s.seen[key] = now.Add(ttl)
	return true, nil
}

// Verifier 校验器。
type Verifier struct {
	keys     map[string]string
	skew     time.Duration
	nonceTTL time.Duration
	nonces   NonceStore
	now      func() time.Time
}

// Key 一把密钥。
type Key struct {
	KeyID  string
	Secret string
}

// NewVerifier 只加载 id 合法、secret ≥MinSecretLen 的 key；返回被跳过的 key id（调用方 WARN）。没有可用 key 时返回 nil。
func NewVerifier(keys []Key, skew, nonceTTL time.Duration, nonces NonceStore) (*Verifier, []string) {
	v := &Verifier{keys: map[string]string{}, skew: skew, nonceTTL: nonceTTL, nonces: nonces, now: time.Now}
	var skipped []string
	for _, k := range keys {
		id := strings.TrimSpace(k.KeyID)
		if !keyIDRe.MatchString(id) || len(k.Secret) < MinSecretLen {
			skipped = append(skipped, id)
			continue
		}
		v.keys[id] = k.Secret
	}
	if len(v.keys) == 0 || nonces == nil {
		return nil, skipped
	}
	if v.skew <= 0 {
		v.skew = 300 * time.Second
	}
	if min := 2*v.skew + nonceGrace; v.nonceTTL < min {
		v.nonceTTL = min // nonce 至少要活过整个时间窗（±skew）再加余量，否则窗口内可重放
	}
	return v, skipped
}

// SetClock 测试用。
func (v *Verifier) SetClock(now func() time.Time) { v.now = now }

// Error 校验失败。
type Error struct {
	Reason string
	Msg    string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Msg }

// Identity 校验通过的调用方。
type Identity struct {
	KeyID  string
	UserID int64
}

// Verify 校验请求（body 为网关收到的原始请求体）。requireUser=true 时 user id 必须 >0。
func (v *Verifier) Verify(ctx context.Context, r *http.Request, body []byte, requireUser bool) (*Identity, error) {
	h := r.Header
	keyID, ts, nonce, uidRaw, sig := strings.TrimSpace(h.Get(HeaderKeyID)), strings.TrimSpace(h.Get(HeaderTimestamp)),
		strings.TrimSpace(h.Get(HeaderNonce)), strings.TrimSpace(h.Get(HeaderUserID)), strings.TrimSpace(h.Get(HeaderSignature))
	if keyID == "" || ts == "" || nonce == "" || sig == "" {
		return nil, &Error{ReasonMissing, "缺少服务间鉴权头"}
	}
	if !keyIDRe.MatchString(keyID) || !nonceRe.MatchString(nonce) {
		return nil, &Error{ReasonMissing, "服务间鉴权头格式不对"}
	}
	uid, err := strconv.ParseInt(uidRaw, 10, 64)
	if uidRaw == "" || err != nil || uid < 0 || (requireUser && uid == 0) {
		return nil, &Error{ReasonUserMissing, "缺少或无效的 X-Svc-User-Id"}
	}
	secret, ok := v.keys[keyID]
	if !ok {
		return nil, &Error{ReasonUnknownKey, "未知的服务密钥"}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, &Error{ReasonMissing, "X-Svc-Timestamp 必须是 unix 秒"}
	}
	if d := v.now().Sub(time.Unix(sec, 0)); d > v.skew || d < -v.skew {
		return nil, &Error{ReasonSkew, "请求时间戳超出允许范围"}
	}
	want := Sign(secret, r.Method, r.URL.Path, ts, nonce, uidRaw, body)
	got, err := hex.DecodeString(strings.ToLower(sig))
	wantRaw, _ := hex.DecodeString(want)
	if err != nil || !hmac.Equal(got, wantRaw) {
		return nil, &Error{ReasonBadSignature, "签名不对"}
	}
	first, err := v.nonces.Claim(ctx, "canvas:svc:nonce:"+keyID+":"+nonce, v.nonceTTL)
	if err != nil {
		return nil, &Error{ReasonNonceStore, "鉴权服务暂不可用：" + err.Error()}
	}
	if !first {
		return nil, &Error{ReasonReplay, "重复的请求（nonce 已使用）"}
	}
	return &Identity{KeyID: keyID, UserID: uid}, nil
}

// IsServerSide 失败是否服务端原因（nonce 存储不可用）。
func IsServerSide(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Reason == ReasonNonceStore
}
