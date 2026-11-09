package userquota

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return mr, rc
}

func at(l *Limiter, ts time.Time) { l.now = func() time.Time { return ts } }

// 每分钟上限：两个实例（两个 Limiter 共用一台 Redis）合计计数；下一分钟恢复；别的用户不受影响。
func TestRedisMinuteLimitSharedAcrossInstances(t *testing.T) {
	_, rc := newRedis(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 7, 10, 0, 5, 0, cst)
	a, b := New("studio:pose_preview", 3, 100, rc), New("studio:pose_preview", 3, 100, rc)
	at(a, t0)
	at(b, t0)
	for i, l := range []*Limiter{a, b, a} {
		if r := l.Take(ctx, 9); r != Allowed {
			t.Fatalf("第 %d 次应放行: %v", i+1, r)
		}
	}
	if r := b.Take(ctx, 9); r != MinuteExceed {
		t.Fatalf("第 4 次应分钟超限: %v", r)
	}
	if r := a.Take(ctx, 10); r != Allowed {
		t.Fatalf("别的用户不受影响: %v", r)
	}
	at(a, t0.Add(time.Minute))
	if r := a.Take(ctx, 9); r != Allowed {
		t.Fatalf("下一分钟应恢复: %v", r)
	}
}

// 每天上限：跨分钟累计，到上限后整天拒绝；分钟级被拒的请求不计入当天额度；北京时间零点后恢复。
func TestRedisDayLimit(t *testing.T) {
	mr, rc := newRedis(t)
	ctx := context.Background()
	l := New("q", 2, 3, rc)
	t0 := time.Date(2026, 11, 7, 23, 50, 0, 0, cst)
	at(l, t0)
	l.Take(ctx, 1)
	l.Take(ctx, 1)
	if r := l.Take(ctx, 1); r != MinuteExceed { // 被拒，不计数
		t.Fatalf("应分钟超限: %v", r)
	}
	at(l, t0.Add(time.Minute))
	if r := l.Take(ctx, 1); r != Allowed {
		t.Fatalf("第 3 次（当天）应放行: %v", r)
	}
	if r := l.Take(ctx, 1); r != DayExceed {
		t.Fatalf("应日超限: %v", r)
	}
	if ttl := mr.TTL("go:quota:q:1:d:20261107"); ttl <= 0 || ttl > dayTTL {
		t.Fatalf("日计数要带过期: %v", ttl)
	}
	at(l, time.Date(2026, 11, 8, 0, 0, 1, 0, cst))
	if r := l.Take(ctx, 1); r != Allowed {
		t.Fatalf("次日应恢复: %v", r)
	}
}

// Redis 不可用：放行（不因 Redis 抖动关掉功能）。
func TestRedisDownFailsOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rc.Close() })
	l := New("q", 1, 1, rc)
	mr.Close()
	for i := 0; i < 3; i++ {
		if r := l.Take(context.Background(), 1); r != Allowed {
			t.Fatalf("Redis 不可用应放行: %v", r)
		}
	}
}

// 没配 Redis：进程内计数，口径相同；上限 <= 0 不限；nil Limiter 放行。
func TestMemoryFallbackAndUnlimited(t *testing.T) {
	ctx := context.Background()
	l := New("q", 2, 0, nil)
	if l.Shared() {
		t.Fatal("没配 Redis 不应共享")
	}
	if l.Take(ctx, 1) != Allowed || l.Take(ctx, 1) != Allowed || l.Take(ctx, 1) != MinuteExceed {
		t.Fatal("进程内分钟上限不对")
	}
	if New("q", 0, 0, nil).Take(ctx, 1) != Allowed {
		t.Fatal("不限应放行")
	}
	var nl *Limiter
	if nl.Take(ctx, 1) != Allowed {
		t.Fatal("nil 应放行")
	}
}

// 退还：命中缓存的请求不占额度；退不到负数；nil / 不限无操作。
func TestRefund(t *testing.T) {
	_, rds := newRedis(t)
	l := New("t", 2, 0, rds)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if r := l.Take(ctx, 1); r != Allowed {
			t.Fatalf("第 %d 次应放行: %v", i, r)
		}
		l.Refund(ctx, 1)
	}
	l.Take(ctx, 1)
	l.Take(ctx, 1)
	if r := l.Take(ctx, 1); r != MinuteExceed {
		t.Fatalf("两次后应超限: %v", r)
	}
	l.Refund(ctx, 2) // 没计数的用户不会减成负数
	if r := l.Take(ctx, 2); r != Allowed {
		t.Fatal(r)
	}
	var nilL *Limiter
	nilL.Refund(ctx, 1)
	m := New("m", 1, 0, nil)
	m.Take(ctx, 1)
	m.Refund(ctx, 1)
	if r := m.Take(ctx, 1); r != Allowed {
		t.Fatalf("进程内退还: %v", r)
	}
}
