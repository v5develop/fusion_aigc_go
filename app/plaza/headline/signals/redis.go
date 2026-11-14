// Package signals 头条的 Redis 实时计数（§14.4.4）、榜单缓存、刷新互斥。
//
// 键前缀 aigc:plaza:（与 Java 的 fusion: 隔离）：
//
//	hl:h:{yyyyMMddHH}                     HASH  field=模板 id，值=该小时计入数（小时按 CST 取自轮次 create_time）   TTL 15 天
//	hl:uv:{yyyyMMdd}:{tpl}                HLL   计入过的用户（7 天去重 = 多 key PFCOUNT）                          TTL 8 天
//	hl:cap:{yyyyMMdd}:{tpl}:{uid}         STR   当日计数（封顶）                                                    TTL 2 天
//	hl:last:{tpl}:{uid}                   STR   最近一次尝试的 epoch ms（快速重复判断）                              TTL rapid×2
//	hl:seen:{turnRowId}                   STR   摄入幂等（崩溃重读同一批不重复计数）                                TTL 1 天
//	headlines:latest:{scope}              STR   每个行业范围的最新榜单（JSON），刷新后逐范围覆盖                    TTL 1 小时
//	headlines:latest:ver:{scope}          STR   上面那份榜单的版本号（回填按它比较新旧，旧版不覆盖新版）            TTL 同上
//	headlines:refresh:lock / :dirty       STR   刷新互斥 / 运营触发时的脏标记
package signals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"aigc-local-life/app/plaza/headline/algo"

	"github.com/redis/go-redis/v9"
)

// Prefix 键前缀。
const Prefix = "aigc:plaza:"

const (
	hourTTL = 15 * 24 * time.Hour // 再往前 7 天（prevUses / trend）要读第 8~14 天的桶
	uvTTL   = 8 * 24 * time.Hour
	capTTL  = 2 * 24 * time.Hour
	seenTTL = 24 * time.Hour
)

// HourKey 小时桶键。
func HourKey(t time.Time) string { return Prefix + "hl:h:" + algo.HourLabel(t) }

// UVKey 日去重用户键。
func UVKey(day string, tpl int64) string {
	return Prefix + "hl:uv:" + day + ":" + strconv.FormatInt(tpl, 10)
}

func capKey(day string, tpl, uid int64) string {
	return Prefix + "hl:cap:" + day + ":" + strconv.FormatInt(tpl, 10) + ":" + strconv.FormatInt(uid, 10)
}

func lastKey(tpl, uid int64) string {
	return Prefix + "hl:last:" + strconv.FormatInt(tpl, 10) + ":" + strconv.FormatInt(uid, 10)
}

func seenKey(rowID int64) string { return Prefix + "hl:seen:" + strconv.FormatInt(rowID, 10) }

// LatestKey 某行业范围的最新榜单缓存键。ALL 不进用户侧缓存。
func LatestKey(scope string) string { return Prefix + "headlines:latest:" + scope }

// 刷新互斥与脏标记。
const (
	LockKey  = Prefix + "headlines:refresh:lock"
	DirtyKey = Prefix + "headlines:refresh:dirty"
	// CatalogVerKey 行业模块的目录版本号（后台写模板 / 行业 / 关系时递增）；变了就重载目录
	CatalogVerKey = Prefix + "catalog:ver"
)

// ingestScript 原子摄入一次尝试（与 algo.CountTurns 同语义）：
//
//	seen 已存在 → 0；与上一次尝试相差 < rapid → 刷新 last、返回 0；否则当日计数 +1，超封顶 → 0；
//	否则小时桶 +1、PFADD 用户、返回 1。last 取 max(旧值, 本次)，乱序到达不回退。
var ingestScript = redis.NewScript(`
if redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[6]) == false then return 0 end
local ts = tonumber(ARGV[1])
local last = redis.call('GET', KEYS[2])
local newest = ts
if last then
  local l = tonumber(last)
  if l > newest then newest = l end
end
redis.call('SET', KEYS[2], tostring(newest), 'PX', ARGV[7])
if last and tonumber(ARGV[2]) > 0 and math.abs(ts - tonumber(last)) < tonumber(ARGV[2]) then return 0 end
local c = redis.call('INCR', KEYS[3])
if c == 1 then redis.call('EXPIRE', KEYS[3], ARGV[8]) end
if c > tonumber(ARGV[3]) then return 0 end
redis.call('HINCRBY', KEYS[4], ARGV[4], 1)
redis.call('EXPIRE', KEYS[4], ARGV[9])
redis.call('PFADD', KEYS[5], ARGV[5])
redis.call('EXPIRE', KEYS[5], ARGV[10])
return 1
`)

// Store Redis 计数与缓存。rdb 为 nil 时所有方法返回 ErrNoRedis（调用方降级 DB / 进程内锁）。
type Store struct{ rdb redis.UniversalClient }

// ErrNoRedis 未配置 Redis。
var ErrNoRedis = errors.New("plaza headline: 未配置 Redis")

// New 包装 Redis 客户端（可为 nil）。
func New(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

// Enabled 是否有 Redis。
func (s *Store) Enabled() bool { return s != nil && s.rdb != nil }

// Client 底层客户端（可能为 nil）。
func (s *Store) Client() redis.UniversalClient {
	if s == nil {
		return nil
	}
	return s.rdb
}

// Ingest 摄入一批尝试，返回计入的条数。整批流水线执行；任一错误整体返回（水位不推进，下轮重读，seen 保证不重复）。
func (s *Store) Ingest(ctx context.Context, turns []algo.Turn, dailyCap, rapidSeconds int) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoRedis
	}
	if len(turns) == 0 {
		return 0, nil
	}
	if err := ingestScript.Load(ctx, s.rdb).Err(); err != nil {
		return 0, err
	}
	lastTTL := int64(rapidSeconds) * 2000
	if lastTTL < 1000 {
		lastTTL = 1000
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.Cmd, 0, len(turns))
	for _, t := range turns {
		if t.TemplateID <= 0 {
			continue
		}
		day := algo.DayLabel(t.CreatedAt)
		keys := []string{seenKey(t.RowID), lastKey(t.TemplateID, t.UserID), capKey(day, t.TemplateID, t.UserID), HourKey(t.CreatedAt), UVKey(day, t.TemplateID)}
		args := []any{t.CreatedAt.UnixMilli(), int64(rapidSeconds) * 1000, dailyCap, t.TemplateID, t.UserID,
			int64(seenTTL / time.Second), lastTTL, int64(capTTL / time.Second), int64(hourTTL / time.Second), int64(uvTTL / time.Second)}
		cmds = append(cmds, ingestScript.EvalSha(ctx, pipe, keys, args...))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	n := 0
	for _, c := range cmds {
		if v, err := c.Int(); err == nil && v == 1 {
			n++
		}
	}
	return n, nil
}

// ReadSignals 读近 14 天的小时桶与近 7 天去重用户，组装成每个模板的信号（所有范围共享一次读取）。
func (s *Store) ReadSignals(ctx context.Context, now time.Time) (map[int64]algo.Signal, error) {
	if !s.Enabled() {
		return nil, ErrNoRedis
	}
	cur := now.Truncate(time.Hour)
	const hours = 2 * algo.HoursPerWeek
	pipe := s.rdb.Pipeline()
	hcmds := make([]*redis.MapStringStringCmd, hours)
	hkeys := make([]int64, hours)
	for i := 0; i < hours; i++ {
		h := cur.Add(-time.Duration(i) * time.Hour)
		hkeys[i] = algo.HourKey(h)
		hcmds[i] = pipe.HGetAll(ctx, HourKey(h))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := map[int64]algo.Signal{}
	for i, c := range hcmds {
		for field, val := range c.Val() {
			tpl, err1 := strconv.ParseInt(field, 10, 64)
			n, err2 := strconv.Atoi(val)
			if err1 != nil || err2 != nil || n <= 0 {
				continue
			}
			sig, ok := out[tpl]
			if !ok {
				sig = algo.Signal{TemplateID: tpl, Hours: map[int64]int{}}
			}
			sig.Hours[hkeys[i]] += n
			out[tpl] = sig
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	days := make([]string, 7)
	for i := range days {
		days[i] = algo.DayLabel(now.AddDate(0, 0, -i))
	}
	pipe = s.rdb.Pipeline()
	ucmds := map[int64]*redis.IntCmd{}
	for tpl := range out {
		keys := make([]string, len(days))
		for i, d := range days {
			keys[i] = UVKey(d, tpl)
		}
		ucmds[tpl] = pipe.PFCount(ctx, keys...)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	for tpl, c := range ucmds {
		sig := out[tpl]
		sig.UniqueUsers = int(c.Val())
		out[tpl] = sig
	}
	return out, nil
}

// Calibrate 用 DB 聚合覆盖已完成的小时桶（当前未完成的小时不动，避免与实时写入竞争），并把计入用户补进 HLL。
// 返回漂移 Σ|redis − db|（持续偏大要告警）。
func (s *Store) Calibrate(ctx context.Context, counts algo.TurnCounts, from, now time.Time) (drift int, err error) {
	if !s.Enabled() {
		return 0, ErrNoRedis
	}
	cur := now.Truncate(time.Hour)
	byHour := map[int64]map[int64]int{}
	for tpl, hours := range counts.Hours {
		for h, c := range hours {
			if byHour[h] == nil {
				byHour[h] = map[int64]int{}
			}
			byHour[h][tpl] = c
		}
	}
	for h := from.Truncate(time.Hour); h.Before(cur); h = h.Add(time.Hour) {
		key := HourKey(h)
		old, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return drift, err
		}
		want := byHour[algo.HourKey(h)]
		for field, val := range old {
			tpl, _ := strconv.ParseInt(field, 10, 64)
			n, _ := strconv.Atoi(val)
			drift += abs(n - want[tpl])
		}
		for tpl, c := range want {
			if _, ok := old[strconv.FormatInt(tpl, 10)]; !ok {
				drift += c
			}
		}
		if len(old) == 0 && len(want) == 0 {
			continue
		}
		pipe := s.rdb.TxPipeline()
		pipe.Del(ctx, key)
		if len(want) > 0 {
			vals := make([]any, 0, len(want)*2)
			for tpl, c := range want {
				vals = append(vals, strconv.FormatInt(tpl, 10), c)
			}
			pipe.HSet(ctx, key, vals...)
			pipe.Expire(ctx, key, hourTTL-now.Sub(h))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return drift, err
		}
	}
	pipe := s.rdb.Pipeline()
	for tpl, days := range counts.DayUsers {
		for day, users := range days {
			if len(users) == 0 {
				continue
			}
			vals := make([]any, 0, len(users))
			for u := range users {
				vals = append(vals, u)
			}
			key := UVKey(day, tpl)
			pipe.PFAdd(ctx, key, vals...)
			pipe.Expire(ctx, key, uvTTL)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return drift, err
	}
	return drift, nil
}

// HasAnyBucket 近 7 天是否有任何小时桶（Redis 被清空 / 新环境时先校准再刷新）。
func (s *Store) HasAnyBucket(ctx context.Context, now time.Time) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoRedis
	}
	cur := now.Truncate(time.Hour)
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.IntCmd, algo.HoursPerWeek)
	for i := range cmds {
		cmds[i] = pipe.Exists(ctx, HourKey(cur.Add(-time.Duration(i)*time.Hour)))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	for _, c := range cmds {
		if c.Val() > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Fence 写入前要比对的刷新锁（键 + 本次持有的令牌）。nil = 不比对（用户端回填）。
type Fence struct {
	Key   string
	Token string
}

// ErrLockLost 刷新锁已不属于本次刷新（过期被别的副本拿走）：本次结果作废，不写缓存。
var ErrLockLost = errors.New("plaza headline: 刷新锁已不属于本次刷新")

// latestVerKey 某范围最新榜单缓存对应的版本号（与榜单同 TTL；Lua 按它比较新旧）。
func latestVerKey(scope string) string { return Prefix + "headlines:latest:ver:" + scope }

// setLatestScript 按版本号比较后写（L9）+ 可选的锁令牌比对（M7），原子执行：
//
//	KEYS[1]=锁键（空串 = 不比对），之后每两个键一组 {榜单键, 版本键}；ARGV[1]=令牌，ARGV[2]=版本号，ARGV[3]=TTL 毫秒，之后逐组榜单 JSON。
//	锁不属于自己 → -1；缓存里的版本号比本次新 → 该组跳过；返回写入的组数。
var setLatestScript = redis.NewScript(`
if KEYS[1] ~= '' and redis.call('GET', KEYS[1]) ~= ARGV[1] then return -1 end
local ver = tonumber(ARGV[2])
local n = 0
for i = 2, #KEYS, 2 do
  local cur = redis.call('GET', KEYS[i+1])
  if (not cur) or tonumber(cur) <= ver then
    redis.call('SET', KEYS[i], ARGV[3 + i/2], 'PX', ARGV[3])
    redis.call('SET', KEYS[i+1], ARGV[2], 'PX', ARGV[3])
    n = n + 1
  end
end
return n
`)

// SetLatest 逐范围覆盖最新榜单缓存（不是失效重建，避免击穿）。只在缓存里的版本号不比 version 新时写，
// 避免慢请求把旧版本回填到新版本之上；fence 非 nil 时先比对刷新锁令牌，不属于自己返回 ErrLockLost、一个都不写。
// 返回实际写入的范围数（被更新版本挡住的不算）。
func (s *Store) SetLatest(ctx context.Context, payloads map[string][]byte, version int64, ttl time.Duration, fence *Fence) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoRedis
	}
	lockKey, token := "", ""
	if fence != nil {
		lockKey, token = fence.Key, fence.Token
	}
	keys := []string{lockKey}
	args := []any{token, version, ttl.Milliseconds()}
	for scope, b := range payloads {
		if scope == algo.ScopeAll {
			continue
		}
		keys = append(keys, LatestKey(scope), latestVerKey(scope))
		args = append(args, b)
	}
	if len(keys) == 1 && fence == nil {
		return 0, nil
	}
	n, err := setLatestScript.Run(ctx, s.rdb, keys, args...).Int()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, ErrLockLost
	}
	return n, nil
}

// GetLatest 读某范围最新榜单缓存；没有返回 nil, nil。
func (s *Store) GetLatest(ctx context.Context, scope string) ([]byte, error) {
	if !s.Enabled() {
		return nil, ErrNoRedis
	}
	b, err := s.rdb.Get(ctx, LatestKey(scope)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return b, err
}

// TryLock SET NX PX 互斥；返回令牌（释放时比对，不误删别人的锁）。
func (s *Store) TryLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	if !s.Enabled() {
		return "", false, ErrNoRedis
	}
	token := lockToken()
	ok, err := s.rdb.SetNX(ctx, key, token, ttl).Result()
	return token, ok, err
}

var (
	unlockScript = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)
	renewScript  = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end return 0`)
	holdsScript  = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return 1 end return 0`)
)

// RenewLock 锁仍属于自己时续期到 ttl；ok=false 表示锁已过期 / 被别人拿走。
func (s *Store) RenewLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoRedis
	}
	n, err := renewScript.Run(ctx, s.rdb, []string{key}, token, ttl.Milliseconds()).Int()
	return n == 1, err
}

// HoldsLock 锁是否仍属于该令牌（Lua 比对，读写之间不被插队）。
func (s *Store) HoldsLock(ctx context.Context, key, token string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoRedis
	}
	n, err := holdsScript.Run(ctx, s.rdb, []string{key}, token).Int()
	return n == 1, err
}

// Unlock 只释放自己持有的锁。
func (s *Store) Unlock(ctx context.Context, key, token string) {
	if s.Enabled() {
		_ = unlockScript.Run(ctx, s.rdb, []string{key}, token).Err()
	}
}

// MarkDirty 置脏标记（刷新进行中时运营又改了规则：当前刷新结束后再跑一次；也是网关请求「新范围无榜」的异步触发）。
func (s *Store) MarkDirty(ctx context.Context) error {
	if !s.Enabled() {
		return ErrNoRedis
	}
	return s.rdb.Set(ctx, DirtyKey, "1", 30*time.Minute).Err()
}

// TakeDirty 取走脏标记（GETDEL）。
func (s *Store) TakeDirty(ctx context.Context) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoRedis
	}
	_, err := s.rdb.GetDel(ctx, DirtyKey).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	return err == nil, err
}

// CatalogVersion 行业模块的目录版本号；没有返回空串。
func (s *Store) CatalogVersion(ctx context.Context) (string, error) {
	if !s.Enabled() {
		return "", ErrNoRedis
	}
	v, err := s.rdb.Get(ctx, CatalogVerKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// lockToken 随机令牌（纳秒时间戳在多副本同时抢锁时可能撞，再拼 8 字节随机数）。
func lockToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + hex.EncodeToString(b[:])
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
