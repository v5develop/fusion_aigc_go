package lease

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

var (
	qAcquire = regexp.QuoteMeta("UPDATE `afv_t` SET `lease_owner` = ?, `lease_until` = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), `updated_at` = `updated_at` WHERE `id` = ? AND (`lease_until` IS NULL OR `lease_until` < NOW(3))")
	qExtend  = regexp.QuoteMeta("UPDATE `afv_t` SET `lease_until` = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), `updated_at` = `updated_at` WHERE `id` = ? AND `lease_owner` = ?")
	qRelease = regexp.QuoteMeta("UPDATE `afv_t` SET `lease_owner` = NULL, `lease_until` = NULL, `updated_at` = `updated_at` WHERE `id` = ? AND `lease_owner` = ?")
)

// 两个实例先后抢同一行：第一次 RowsAffected=1 抢到，第二次 0（租约未过期）抢不到。
func TestAcquireMutualExclusionSQL(t *testing.T) {
	db, mock := newMock(t)
	ctx := context.Background()
	mock.ExpectExec(qAcquire).WithArgs("a", int64(180_000_000), int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qAcquire).WithArgs("b", int64(180_000_000), int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	if ok, err := Acquire(ctx, db, "afv_t", 7, "a", 3*time.Minute); err != nil || !ok {
		t.Fatalf("第一次应抢到: ok=%v err=%v", ok, err)
	}
	if ok, err := Acquire(ctx, db, "afv_t", 7, "b", 3*time.Minute); err != nil || ok {
		t.Fatalf("第二次应抢不到: ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExtendReleaseSQL(t *testing.T) {
	db, mock := newMock(t)
	ctx := context.Background()
	mock.ExpectExec(qExtend).WithArgs(int64(600_000_000), int64(7), "a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qExtend).WithArgs(int64(600_000_000), int64(7), "b").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(qRelease).WithArgs(int64(7), "a").WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, _ := Extend(ctx, db, "afv_t", 7, "a", 10*time.Minute); !ok {
		t.Fatal("owner 续租应成功")
	}
	if ok, _ := Extend(ctx, db, "afv_t", 7, "b", 10*time.Minute); ok {
		t.Fatal("非 owner 续租应失败")
	}
	if err := Release(ctx, db, "afv_t", 7, "a"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	db, _ := newMock(t)
	if _, err := Acquire(context.Background(), db, "t; DROP", 1, "a", time.Minute); err == nil {
		t.Fatal("非法表名应拒")
	}
	if _, err := Acquire(context.Background(), db, "t", 1, "", time.Minute); err == nil {
		t.Fatal("空 owner 应拒")
	}
	if _, err := Acquire(context.Background(), db, "t", 1, "a", time.Millisecond); err == nil {
		t.Fatal("ttl 太短应拒")
	}
}

func TestOwner(t *testing.T) {
	o := Owner()
	if o == "" || len(o) > 64 || o != Owner() || !strings.Contains(o, "-") {
		t.Fatalf("owner 不合规: %q", o)
	}
}

// ---------------- 语义：内存里模拟一张表（库时钟可拨） ----------------

type row struct {
	owner string
	until time.Time // 零值 = NULL
}

type fakeDB struct {
	mu        sync.Mutex
	now       time.Time
	rows      map[int64]*row
	multi     int   // ExtendMany 调用次数
	extendErr error // 非空时 Extend 返回该错误（模拟库不可用）
	multiErr  error // 非空时 ExtendMany 返回该错误
}

func newFake() *fakeDB {
	return &fakeDB{now: time.Unix(1_800_000_000, 0), rows: map[int64]*row{7: {}}}
}

func (f *fakeDB) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

type result int64

func (r result) LastInsertId() (int64, error) { return 0, nil }
func (r result) RowsAffected() (int64, error) { return int64(r), nil }

func (f *fakeDB) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "UPDATE `afv_t` SET `lease_owner` = ?"): // Acquire
		owner, us, id := args[0].(string), args[1].(int64), args[2].(int64)
		r := f.rows[id]
		if r == nil || (!r.until.IsZero() && !r.until.Before(f.now)) {
			return result(0), nil
		}
		r.owner, r.until = owner, f.now.Add(time.Duration(us)*time.Microsecond)
		return result(1), nil
	case strings.Contains(q, "`id` IN ("): // ExtendMany
		if f.multiErr != nil {
			return nil, f.multiErr
		}
		us, owner := args[0].(int64), args[len(args)-1].(string)
		f.multi++
		n := 0
		for _, a := range args[1 : len(args)-1] {
			if r := f.rows[a.(int64)]; r != nil && r.owner == owner {
				r.until = f.now.Add(time.Duration(us) * time.Microsecond)
				n++
			}
		}
		return result(n), nil
	case strings.HasPrefix(q, "UPDATE `afv_t` SET `lease_until` ="): // Extend
		if f.extendErr != nil {
			return nil, f.extendErr
		}
		us, id, owner := args[0].(int64), args[1].(int64), args[2].(string)
		r := f.rows[id]
		if r == nil || r.owner != owner {
			return result(0), nil
		}
		r.until = f.now.Add(time.Duration(us) * time.Microsecond)
		return result(1), nil
	case strings.HasPrefix(q, "UPDATE `afv_t` SET `lease_owner` = NULL"): // Release
		id, owner := args[0].(int64), args[1].(string)
		r := f.rows[id]
		if r == nil || r.owner != owner {
			return result(0), nil
		}
		r.owner, r.until = "", time.Time{}
		return result(1), nil
	}
	panic("unexpected sql: " + q)
}

func TestExpiredLeaseCanBeTakenOver(t *testing.T) {
	db, ctx := newFake(), context.Background()
	if ok, _ := Acquire(ctx, db, "afv_t", 7, "a", 3*time.Minute); !ok {
		t.Fatal("a 应抢到")
	}
	db.advance(2 * time.Minute)
	if ok, _ := Acquire(ctx, db, "afv_t", 7, "b", 3*time.Minute); ok {
		t.Fatal("未过期 b 不应抢到")
	}
	if ok, _ := Acquire(ctx, db, "afv_t", 7, "a", 3*time.Minute); ok {
		t.Fatal("不可重入：a 自己也不应再抢到")
	}
	db.advance(2 * time.Minute) // a 崩溃，没续租也没释放 → 过期
	if ok, _ := Acquire(ctx, db, "afv_t", 7, "b", 3*time.Minute); !ok {
		t.Fatal("过期后 b 应能接管")
	}
	if ok, _ := Extend(ctx, db, "afv_t", 7, "a", 3*time.Minute); ok {
		t.Fatal("被接管后 a 的续租应失败")
	}
	if err := Release(ctx, db, "afv_t", 7, "a"); err != nil || db.rows[7].owner != "b" {
		t.Fatalf("a 的释放不应影响 b: %+v", db.rows[7])
	}
}

func TestExtendOnlyOwner(t *testing.T) {
	db, ctx := newFake(), context.Background()
	_, _ = Acquire(ctx, db, "afv_t", 7, "a", 3*time.Minute)
	if ok, _ := Extend(ctx, db, "afv_t", 7, "b", time.Hour); ok {
		t.Fatal("b 不应能续 a 的租约")
	}
	db.advance(2 * time.Minute)
	if ok, _ := Extend(ctx, db, "afv_t", 7, "a", 3*time.Minute); !ok {
		t.Fatal("a 应能续租")
	}
	db.advance(2 * time.Minute) // 续过之后，从初次抢到算已 4 分钟，仍未过期
	if ok, _ := Acquire(ctx, db, "afv_t", 7, "b", 3*time.Minute); ok {
		t.Fatal("续租后 b 不应抢到")
	}
	if err := Release(ctx, db, "afv_t", 7, "a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Acquire(ctx, db, "afv_t", 7, "b", 3*time.Minute); !ok {
		t.Fatal("释放后 b 应立刻抢到")
	}
}

func TestKeepCancelsWhenLost(t *testing.T) {
	db := newFake()
	ctx2, stop, ok, err := Keep(context.Background(), db, "afv_t", 7, "a", 3*time.Second)
	if err != nil || !ok {
		t.Fatalf("应抢到: %v", err)
	}
	if _, _, ok, _ := Keep(context.Background(), db, "afv_t", 7, "b", 3*time.Second); ok {
		t.Fatal("b 不应抢到")
	}
	// 模拟被接管：owner 改成 b，下一次心跳（1s）续租失败 → ctx2 取消
	db.mu.Lock()
	db.rows[7].owner = "b"
	db.mu.Unlock()
	select {
	case <-ctx2.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("丢失租约后 ctx 应取消")
	}
	stop()
	if db.rows[7].owner != "b" {
		t.Fatal("a 的 stop 不应释放 b 的租约")
	}
}

// 续租一直报错（库不可用）：不立刻取消，距上次续上超过 ttl - ttl/3 时主动取消，避免租约在库里过期后两边同时推进。
func TestKeepCancelsWhenExtendKeepsFailing(t *testing.T) {
	db := newFake()
	db.extendErr = errors.New("db down")
	start := time.Now()
	ctx2, stop, ok, err := Keep(context.Background(), db, "afv_t", 7, "a", 3*time.Second)
	if err != nil || !ok {
		t.Fatalf("应抢到: %v", err)
	}
	defer stop()
	select {
	case <-ctx2.Done():
		if d := time.Since(start); d < 1900*time.Millisecond {
			t.Fatalf("第一次续租失败就取消太早了: %v", d)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("续租持续失败超过 ttl-ttl/3 应取消 ctx")
	}
}

// 续租偶发报错、随后恢复：不取消。
func TestKeepToleratesTransientExtendError(t *testing.T) {
	db := newFake()
	db.extendErr = errors.New("db blip")
	ctx2, stop, ok, _ := Keep(context.Background(), db, "afv_t", 7, "a", 3*time.Second)
	if !ok {
		t.Fatal("应抢到")
	}
	defer stop()
	time.Sleep(1200 * time.Millisecond) // 第一次心跳失败
	db.mu.Lock()
	db.extendErr = nil
	db.mu.Unlock()
	select {
	case <-ctx2.Done():
		t.Fatal("续租恢复后不应取消")
	case <-time.After(2500 * time.Millisecond):
	}
}

func TestKeepStopReleases(t *testing.T) {
	db := newFake()
	_, stop, ok, _ := Keep(context.Background(), db, "afv_t", 7, "a", 3*time.Second)
	if !ok {
		t.Fatal("应抢到")
	}
	stop()
	stop() // 幂等
	if db.rows[7].owner != "" {
		t.Fatalf("stop 应释放: %+v", db.rows[7])
	}
}

func TestSet(t *testing.T) {
	db := newFake()
	db.rows[8] = &row{}
	s := NewSet(db, "afv_t", "a", 3*time.Second)
	if ok, _ := s.Acquire(context.Background(), 7); !ok {
		t.Fatal("应抢到 7")
	}
	if ok, _ := s.Acquire(context.Background(), 8); !ok {
		t.Fatal("应抢到 8")
	}
	time.Sleep(1200 * time.Millisecond) // 心跳间隔 ttl/3 = 1s
	db.mu.Lock()
	beats := db.multi
	db.mu.Unlock()
	if beats == 0 {
		t.Fatal("心跳应给持有的租约批量续租")
	}
	s.Release(context.Background(), 7)
	if db.rows[7].owner != "" || db.rows[8].owner != "a" {
		t.Fatal("Release 只释放 7")
	}
	s.Close()
	if db.rows[8].owner != "" {
		t.Fatal("Close 应释放剩余的 8")
	}
}

func waitDone(t *testing.T, ctx context.Context, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(d):
		t.Fatal(what)
	}
}

// 租约被别人持有：心跳发现后取消该行的 ctx，并移出集合（之后 Release 不会去动别人的租约）；别的行不受影响。
func TestSetAcquireCtxCancelsWhenLost(t *testing.T) {
	db := newFake()
	db.rows[8] = &row{}
	s := NewSet(db, "afv_t", "a", 3*time.Second)
	defer s.Close()
	c7, ok, _ := s.AcquireCtx(context.Background(), 7)
	c8, ok8, _ := s.AcquireCtx(context.Background(), 8)
	if !ok || !ok8 {
		t.Fatal("应抢到")
	}
	db.mu.Lock()
	db.rows[7].owner = "b" // 被别人接管
	db.mu.Unlock()
	waitDone(t, c7, 2500*time.Millisecond, "租约丢失应取消 ctx")
	if c8.Err() != nil {
		t.Fatal("没丢的行不应被取消")
	}
	s.Release(context.Background(), 7)
	if db.rows[7].owner != "b" {
		t.Fatal("丢失后的 Release 不能释放别人的租约")
	}
}

// 续租一直出错：超过 ttl - ttl/3 没续上就取消 ctx（留余量给持有者停手）。
func TestSetAcquireCtxCancelsWhenRenewKeepsFailing(t *testing.T) {
	db := newFake()
	db.multiErr = errors.New("db down")
	s := NewSet(db, "afv_t", "a", 3*time.Second)
	defer s.Close()
	c7, ok, _ := s.AcquireCtx(context.Background(), 7)
	if !ok {
		t.Fatal("应抢到")
	}
	time.Sleep(1200 * time.Millisecond)
	if c7.Err() != nil {
		t.Fatal("一次续租失败不应立刻取消")
	}
	waitDone(t, c7, 3*time.Second, "长时间续不上应取消 ctx")
}

// Release 取消 ctx；CloseCtx 尊重截止时间。
func TestSetReleaseCancelsCtx(t *testing.T) {
	db := newFake()
	s := NewSet(db, "afv_t", "a", 3*time.Second)
	c7, _, _ := s.AcquireCtx(context.Background(), 7)
	s.Release(context.Background(), 7)
	if c7.Err() == nil || db.rows[7].owner != "" {
		t.Fatal("Release 应取消 ctx 并释放")
	}
	c7, _, _ = s.AcquireCtx(context.Background(), 7)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.CloseCtx(ctx)
	if c7.Err() == nil || db.rows[7].owner != "" {
		t.Fatal("Close 应取消 ctx 并释放")
	}
}
