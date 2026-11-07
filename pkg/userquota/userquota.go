// Package userquota 按用户的调用频率配额：每分钟 N 次 + 每天 M 次（固定窗口，自然分钟 / 北京时间自然日）。
//
// 配了 Redis 时跨实例共享计数（多实例网关看到同一份额度）；没配时退回进程内计数（只用于本地开发，多实例各算各的）。
// 两个窗口都没到上限才同时 +1（Lua 原子执行）：被拒的请求不占额度，分钟级被拒不会把当天额度一起耗掉。
// Redis 出错时放行并记错误日志（配额只是防滥用，不能因为 Redis 抖动把功能整个关掉）。
package userquota

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// Result 一次申请的结果。
type Result int

const (
	Allowed      Result = iota // 放行（已计数）
	MinuteExceed               // 本分钟已用完
	DayExceed                  // 今天已用完
)

// cst 自然日按北京时间切（FixedZone 不依赖容器里的 tzdata）。
var cst = time.FixedZone("CST", 8*3600)

// Limiter 某一类调用（name 区分，如 "studio:pose_preview"）的按用户配额。PerMinute / PerDay <= 0 表示该窗口不限。
type Limiter struct {
	name      string
	perMinute int
	perDay    int
	rds       redis.UniversalClient // nil = 进程内计数
	now       func() time.Time

	mu  sync.Mutex
	mem map[string]int // 进程内计数：key → 次数（过期 key 在写入时顺手清）
}

// New rds 为 nil 时用进程内计数。
func New(name string, perMinute, perDay int, rds redis.UniversalClient) *Limiter {
	return &Limiter{name: name, perMinute: perMinute, perDay: perDay, rds: rds, now: time.Now, mem: map[string]int{}}
}

// Shared 是否跨实例共享计数（配了 Redis）。
func (l *Limiter) Shared() bool { return l != nil && l.rds != nil }

// keyPrefix 与 Java 共用会话 Redis 库，前缀带 go: 避免撞键。
func (l *Limiter) keys(userID int64, now time.Time) (minute, day string) {
	base := "go:quota:" + l.name + ":" + strconv.FormatInt(userID, 10)
	t := now.In(cst)
	return base + ":m:" + t.Format("200601021504"), base + ":d:" + t.Format("20060102")
}

// TTL 比窗口略长，过期后自然清掉。
const (
	minuteTTL = 2 * time.Minute
	dayTTL    = 48 * time.Hour
)

// takeScript 两个窗口都没到上限才各 +1；上限 <= 0 表示不限。返回 0 放行 / 1 分钟超限 / 2 日超限。
var takeScript = redis.NewScript(`
local m = tonumber(redis.call('GET', KEYS[1]) or '0')
local d = tonumber(redis.call('GET', KEYS[2]) or '0')
local pm, pd = tonumber(ARGV[1]), tonumber(ARGV[2])
if pm > 0 and m >= pm then return 1 end
if pd > 0 and d >= pd then return 2 end
redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[3])
redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[2], ARGV[4])
return 0
`)

// Take 为用户申请一次调用额度。l 为 nil 或两个窗口都不限时恒放行。
func (l *Limiter) Take(ctx context.Context, userID int64) Result {
	if l == nil || (l.perMinute <= 0 && l.perDay <= 0) {
		return Allowed
	}
	mk, dk := l.keys(userID, l.now())
	if l.rds == nil {
		return l.takeMem(mk, dk)
	}
	n, err := takeScript.Run(ctx, l.rds, []string{mk, dk},
		l.perMinute, l.perDay, int(minuteTTL/time.Second), int(dayTTL/time.Second)).Int()
	if err != nil {
		logx.WithContext(ctx).Errorf("userquota: %s 用户 %d 配额计数失败（放行）: %v", l.name, userID, err)
		return Allowed
	}
	return Result(n)
}

func (l *Limiter) takeMem(mk, dk string) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perMinute > 0 && l.mem[mk] >= l.perMinute {
		return MinuteExceed
	}
	if l.perDay > 0 && l.mem[dk] >= l.perDay {
		return DayExceed
	}
	if len(l.mem) > 10000 { // 进程内只为开发兜底：太多就整个清掉重来，不做精确过期
		l.mem = map[string]int{}
	}
	l.mem[mk]++
	l.mem[dk]++
	return Allowed
}
