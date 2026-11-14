package canvasevents

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConnLimiter 每用户 SSE 并发连接计数。
type ConnLimiter interface {
	// Acquire 占一个名额（limit 内返回 true）；ttl 内没 Refresh 的名额视为崩溃残留，自动失效。
	Acquire(ctx context.Context, uid int64, connID string, limit int, ttl time.Duration) (bool, error)
	// Refresh 续期（名额已被当作残留清掉时重新加回）；出错时调用方结束连接，不放行
	Refresh(ctx context.Context, uid int64, connID string, ttl time.Duration) error
	Release(ctx context.Context, uid int64, connID string)
}

// NewConnLimiter 有 Redis 用有序集合（跨实例），否则进程内。
func NewConnLimiter(rdb redis.UniversalClient) ConnLimiter {
	if rdb == nil {
		return &memConns{m: map[int64]map[string]time.Time{}, now: time.Now}
	}
	return &redisConns{r: rdb, now: time.Now}
}

// redisConns ZSET canvas:sse:conns:{uid}：member=connID，score=过期时刻（毫秒）。占名额用 Lua 原子「清过期 → 数 → 加」。
type redisConns struct {
	r   redis.UniversalClient
	now func() time.Time
}

var acquireScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[3]) then
  return 0
end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1
`)

func connKey(uid int64) string { return "canvas:sse:conns:" + strconv.FormatInt(uid, 10) }

func (c *redisConns) Acquire(ctx context.Context, uid int64, connID string, limit int, ttl time.Duration) (bool, error) {
	now := c.now()
	n, err := acquireScript.Run(ctx, c.r, []string{connKey(uid)},
		now.UnixMilli(), now.Add(ttl).UnixMilli(), limit, connID, ttl.Milliseconds()*2).Int()
	return n == 1, err
}

func (c *redisConns) Refresh(ctx context.Context, uid int64, connID string, ttl time.Duration) error {
	k := connKey(uid)
	exp := c.now().Add(ttl).UnixMilli()
	pipe := c.r.Pipeline()
	pipe.ZAdd(ctx, k, redis.Z{Score: float64(exp), Member: connID}) // 不带 XX：被当作残留清掉后也能加回，计数不失真
	pipe.PExpire(ctx, k, 2*ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *redisConns) Release(ctx context.Context, uid int64, connID string) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = c.r.ZRem(ctx, connKey(uid), connID).Err()
}

// memConns 进程内（仅开发 / 单实例）。
type memConns struct {
	mu  sync.Mutex
	m   map[int64]map[string]time.Time
	now func() time.Time
}

func (c *memConns) Acquire(_ context.Context, uid int64, connID string, limit int, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	set := c.m[uid]
	if set == nil {
		set = map[string]time.Time{}
		c.m[uid] = set
	}
	for id, exp := range set {
		if now.After(exp) {
			delete(set, id)
		}
	}
	if len(set) >= limit {
		return false, nil
	}
	set[connID] = now.Add(ttl)
	return true, nil
}

func (c *memConns) Refresh(_ context.Context, uid int64, connID string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.m[uid]
	if set == nil {
		set = map[string]time.Time{}
		c.m[uid] = set
	}
	set[connID] = c.now().Add(ttl)
	return nil
}

func (c *memConns) Release(_ context.Context, uid int64, connID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if set := c.m[uid]; set != nil {
		delete(set, connID)
		if len(set) == 0 {
			delete(c.m, uid)
		}
	}
}
