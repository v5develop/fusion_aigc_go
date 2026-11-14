package signals

import (
	"context"
	"testing"
	"time"

	"aigc-local-life/app/plaza/headline/algo"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return New(redis.NewClient(&redis.Options{Addr: mr.Addr()})), mr
}

var base = time.Date(2026, 11, 13, 9, 0, 0, 0, algo.CST)

func TestIngestCapRapidAndIdempotent(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	var turns []algo.Turn
	for i := 0; i < 5; i++ { // 同用户同模板同一天 5 次（间隔 10 分钟）→ 计 3 次
		turns = append(turns, algo.Turn{RowID: int64(i + 1), UserID: 1, TemplateID: 7, CreatedAt: base.Add(time.Duration(i) * 10 * time.Minute)})
	}
	for i, sec := range []int{0, 50, 100} { // 快速重复：只计 1 次
		turns = append(turns, algo.Turn{RowID: int64(100 + i), UserID: 2, TemplateID: 7, CreatedAt: base.Add(time.Duration(sec) * time.Second)})
	}
	n, err := s.Ingest(ctx, turns, 3, 60)
	if err != nil || n != 4 {
		t.Fatalf("计入=%d err=%v，want 4", n, err)
	}
	// 崩溃重读同一批：seen 幂等，不重复计数
	if n, _ = s.Ingest(ctx, turns, 3, 60); n != 0 {
		t.Fatalf("重放应计入 0, got %d", n)
	}
	sigs, err := s.ReadSignals(ctx, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := sigs[7].Hours[algo.HourKey(base)]; got != 4 || sigs[7].UniqueUsers != 2 {
		t.Fatalf("信号: %+v", sigs[7])
	}
	// 与纯函数口径一致
	pure := algo.CountTurns(turns, 3, 60)
	if pure.Hours[7][algo.HourKey(base)] != 4 {
		t.Fatalf("Lua 与 CountTurns 口径不一致: %v", pure.Hours[7])
	}
}

func TestCalibrateOverwritesCompletedHoursOnly(t *testing.T) {
	s, mr := newStore(t)
	ctx := context.Background()
	now := base.Add(3*time.Hour + 20*time.Minute) // 12:20
	// Redis 里 09 点的桶算多了、11 点漏了，12 点（未完成）有实时写入
	mr.HSet(HourKey(base), "7", "9")
	mr.HSet(HourKey(base.Add(3*time.Hour)), "7", "5")
	counts := algo.CountTurns([]algo.Turn{
		{RowID: 1, UserID: 1, TemplateID: 7, CreatedAt: base},
		{RowID: 2, UserID: 2, TemplateID: 7, CreatedAt: base.Add(2 * time.Hour)},
		{RowID: 3, UserID: 3, TemplateID: 7, CreatedAt: base.Add(3 * time.Hour)},
	}, 3, 60)
	drift, err := s.Calibrate(ctx, counts, base.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if drift != 8+1 {
		t.Fatalf("漂移=%d want 9", drift)
	}
	if v := mr.HGet(HourKey(base), "7"); v != "1" {
		t.Fatalf("已完成小时应被覆盖为 1, got %s", v)
	}
	if v := mr.HGet(HourKey(base.Add(2*time.Hour)), "7"); v != "1" {
		t.Fatalf("漏掉的小时应补上, got %s", v)
	}
	if v := mr.HGet(HourKey(base.Add(3*time.Hour)), "7"); v != "5" {
		t.Fatalf("当前未完成的小时不动, got %s", v)
	}
	sigs, _ := s.ReadSignals(ctx, now)
	if sigs[7].UniqueUsers != 3 {
		t.Fatalf("HLL 应补进计入用户: %d", sigs[7].UniqueUsers)
	}
	if ok, _ := s.HasAnyBucket(ctx, now); !ok {
		t.Fatal("应有桶")
	}
}

func TestLatestLockDirty(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.SetLatest(ctx, map[string][]byte{"foot_bath": []byte(`{"v":1}`), algo.ScopeAll: []byte(`x`)}, 1, time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.GetLatest(ctx, "foot_bath"); string(b) != `{"v":1}` {
		t.Fatalf("latest=%s", b)
	}
	if b, _ := s.GetLatest(ctx, algo.ScopeAll); b != nil {
		t.Fatal("ALL 不进用户侧缓存")
	}
	tok, ok, _ := s.TryLock(ctx, LockKey, time.Minute)
	if !ok {
		t.Fatal("首次应拿到锁")
	}
	if _, ok2, _ := s.TryLock(ctx, LockKey, time.Minute); ok2 {
		t.Fatal("锁应互斥")
	}
	s.Unlock(ctx, LockKey, "other")
	if _, ok2, _ := s.TryLock(ctx, LockKey, time.Minute); ok2 {
		t.Fatal("别人的令牌不能解锁")
	}
	s.Unlock(ctx, LockKey, tok)
	if _, ok2, _ := s.TryLock(ctx, LockKey, time.Minute); !ok2 {
		t.Fatal("解锁后应能再拿")
	}
	if d, _ := s.TakeDirty(ctx); d {
		t.Fatal("初始无脏标记")
	}
	_ = s.MarkDirty(ctx)
	if d, _ := s.TakeDirty(ctx); !d {
		t.Fatal("应取到脏标记")
	}
	if d, _ := s.TakeDirty(ctx); d {
		t.Fatal("取走后应清空")
	}
	var none *Store = New(nil)
	if _, err := none.ReadSignals(ctx, base); err != ErrNoRedis {
		t.Fatal("无 Redis 应返回 ErrNoRedis")
	}
}

// L9：回填按版本号比较后写，旧版不覆盖新版；同版本可重写。
func TestSetLatestVersionCompare(t *testing.T) {
	s, mr := newStore(t)
	ctx := context.Background()
	if n, err := s.SetLatest(ctx, map[string][]byte{"a": []byte(`v5`), "b": []byte(`v5`)}, 5, time.Hour, nil); err != nil || n != 2 {
		t.Fatalf("首写 n=%d err=%v", n, err)
	}
	if n, _ := s.SetLatest(ctx, map[string][]byte{"a": []byte(`v4`)}, 4, time.Hour, nil); n != 0 {
		t.Fatal("旧版本不能覆盖新版本")
	}
	if b, _ := s.GetLatest(ctx, "a"); string(b) != "v5" {
		t.Fatalf("a=%s", b)
	}
	if n, _ := s.SetLatest(ctx, map[string][]byte{"a": []byte(`v6`), "b": []byte(`v4`)}, 6, time.Hour, nil); n != 2 {
		t.Fatal("新版本应覆盖")
	}
	if b, _ := s.GetLatest(ctx, "a"); string(b) != "v6" {
		t.Fatalf("a=%s", b)
	}
	if ttl := mr.TTL(LatestKey("a")); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("TTL=%v", ttl)
	}
	if ttl := mr.TTL(latestVerKey("a")); ttl <= 0 {
		t.Fatalf("版本键应同带 TTL: %v", ttl)
	}
}

// M7：写缓存前比对锁令牌；续期只续自己的锁。
func TestSetLatestFenceAndRenew(t *testing.T) {
	s, mr := newStore(t)
	ctx := context.Background()
	tok, ok, err := s.TryLock(ctx, LockKey, time.Second)
	if err != nil || !ok {
		t.Fatal("应拿到锁")
	}
	if held, _ := s.HoldsLock(ctx, LockKey, tok); !held {
		t.Fatal("应仍持有")
	}
	if ok, _ := s.RenewLock(ctx, LockKey, tok, time.Minute); !ok || mr.TTL(LockKey) < 30*time.Second {
		t.Fatalf("续期应生效 ttl=%v", mr.TTL(LockKey))
	}
	if n, err := s.SetLatest(ctx, map[string][]byte{"a": []byte(`x`)}, 1, time.Hour, &Fence{Key: LockKey, Token: tok}); err != nil || n != 1 {
		t.Fatalf("持锁写 n=%d err=%v", n, err)
	}
	// 锁过期后被别人拿走
	mr.Set(LockKey, "other")
	if ok, _ := s.RenewLock(ctx, LockKey, tok, time.Minute); ok {
		t.Fatal("别人的锁不能续")
	}
	if held, _ := s.HoldsLock(ctx, LockKey, tok); held {
		t.Fatal("令牌已不匹配")
	}
	if _, err := s.SetLatest(ctx, map[string][]byte{"a": []byte(`y`)}, 2, time.Hour, &Fence{Key: LockKey, Token: tok}); err != ErrLockLost {
		t.Fatalf("丢锁应返回 ErrLockLost, got %v", err)
	}
	if b, _ := s.GetLatest(ctx, "a"); string(b) != "x" {
		t.Fatal("丢锁时一个都不写")
	}
	if tok2, _, _ := s.TryLock(ctx, "k2", time.Second); tok2 == tok {
		t.Fatal("令牌应随机")
	}
}
