package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
)

func ctlRows(wm int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "params_json", "params_version", "updated_by", "ingest_last_id", "calibrated_at"}).
		AddRow(1, nil, 0, nil, wm, nil)
}

// M7：Redis 锁出错时跳过本轮（不退回进程内锁、不写快照）。
func TestRefreshSkipsWhenRedisLockErrors(t *testing.T) {
	s, mock, mr := newSvc(t)
	mr.Close()
	mock.ExpectQuery("FROM `afv_plaza_headline_snapshot` AS").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	out, err := s.RefreshNow(context.Background(), TriggerOps, 1)
	if err != nil || out.Refreshed || out.Version != 7 {
		t.Fatalf("应跳过本轮并带当前版本: %+v %v", out, err)
	}
	if !s.refreshMu.TryLock() {
		t.Fatal("不应退回进程内锁")
	}
	s.refreshMu.Unlock()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// stealingCatalog 装载目录时模拟「锁过期被其他副本拿走」。
type stealingCatalog struct {
	c  *store.Catalog
	mr *miniredis.Miniredis
}

func (f stealingCatalog) LoadCatalog(context.Context, time.Time) (*store.Catalog, error) {
	f.mr.Set(signals.LockKey, "someone-else")
	return f.c, nil
}

// M7：写快照前比对令牌，锁已不属于自己就放弃本次结果（不写快照、不写缓存、不删别人的锁）。
func TestRefreshAbandonsWhenLockLost(t *testing.T) {
	s, mock, mr := newSvc(t)
	s.catalog = stealingCatalog{c: testCatalog(), mr: mr}
	mock.ExpectQuery("FROM `afv_plaza_headline_ctl`").WillReturnRows(ctlRows(0))
	mock.ExpectQuery("FROM afv_agent_turn").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "plaza_template_id", "create_time"}))
	mock.ExpectQuery("FROM `afv_plaza_headline` AS").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_plaza_headline_block`").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_plaza_headline_snapshot` AS").WillReturnRows(sqlmock.NewRows([]string{"id"})) // 上一版：无
	mock.ExpectQuery("FROM `afv_plaza_headline_snapshot` AS").WillReturnRows(sqlmock.NewRows([]string{"id"})) // notRefreshed
	out, err := s.RefreshNow(context.Background(), TriggerScheduled, 0)
	if err != nil || out.Refreshed {
		t.Fatalf("丢锁应放弃: %+v %v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有 INSERT 期望：写了就会报未预期的 Exec
		t.Fatal(err)
	}
	if mr.Exists(signals.LatestKey("foot_bath")) {
		t.Fatal("丢锁不能写缓存")
	}
	if v, _ := mr.Get(signals.LockKey); v != "someone-else" {
		t.Fatal("不能删别人的锁")
	}
}

// M7：持锁期间按 TTL/3 续期；被别人拿走后续期失败、标记丢锁。
func TestRefreshLockRenewal(t *testing.T) {
	old := lockTTL
	lockTTL = 300 * time.Millisecond
	defer func() { lockTTL = old }()
	s, _, mr := newSvc(t)
	ctx := context.Background()
	lk, ok, err := s.lock(ctx)
	if err != nil || !ok {
		t.Fatal("应拿到锁")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) { // miniredis 的 TTL 不随真实时间走：用 FastForward 模拟时间流逝
		time.Sleep(50 * time.Millisecond)
		mr.FastForward(50 * time.Millisecond)
	}
	if held, why := lk.stillHeld(ctx); !held {
		t.Fatalf("续期后应仍持有: %s", why)
	}
	mr.Set(signals.LockKey, "other")
	time.Sleep(250 * time.Millisecond)
	held, why := lk.stillHeld(ctx)
	if held || !strings.Contains(why, "续期") && !strings.Contains(why, "不匹配") {
		t.Fatalf("应判丢锁: %v %s", held, why)
	}
	lk.release()
	if v, _ := mr.Get(signals.LockKey); v != "other" {
		t.Fatal("释放不能删别人的锁")
	}
}

// L10：信号 / 校准窗口起点对齐 CST 日界。
func TestSignalWindowStartAlignsCSTDay(t *testing.T) {
	got := signalWindowStart(now0) // 2026-11-13 14:30 CST
	want := time.Date(2026, 10, 30, 0, 0, 0, 0, algo.CST)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// UTC 时钟下也按 CST 日界（UTC 2026-11-12 17:00 = CST 11-13 01:00）
	if got := signalWindowStart(time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)); !got.Equal(want) {
		t.Fatalf("UTC 输入 got %v", got)
	}
	if now0.Sub(got) < time.Duration(2*algo.HoursPerWeek)*time.Hour {
		t.Fatal("起点必须覆盖完整 14 天")
	}
}

// L10 + L11：校准从 CST 日界读起，且只聚合 id ≤ 已摄入水位的行（水位之后的交给摄入，避免重复计数）。
func TestCalibrateBoundedByWatermark(t *testing.T) {
	s, mock, mr := newSvc(t)
	mock.ExpectQuery("FROM `afv_plaza_headline_ctl`").WillReturnRows(ctlRows(77))
	mock.ExpectQuery("FROM `afv_plaza_headline_ctl`").WillReturnRows(ctlRows(77))
	mock.ExpectQuery(`FROM afv_agent_turn WHERE .*create_time >= '2026-10-30 00:00:00.000' AND create_time < .* AND id <= 77`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "plaza_template_id", "create_time"}).
			AddRow(70, 1, 5, now0.Add(-2*time.Hour)))
	mock.ExpectExec("UPDATE `afv_plaza_headline_ctl`").WillReturnResult(sqlmock.NewResult(0, 1))
	j := NewJobs(s)
	j.calibrate(context.Background(), now0)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if v := mr.HGet(signals.HourKey(now0.Add(-2*time.Hour)), "5"); v != "1" {
		t.Fatalf("水位内的行应进小时桶: %q", v)
	}
	// 水位之后的行随后由摄入计入一次：小时桶 = 2，而不是校准也算一次的 3
	if _, err := s.sig.Ingest(context.Background(), []algo.Turn{{RowID: 78, UserID: 2, TemplateID: 5, CreatedAt: now0.Add(-2 * time.Hour)}}, 3, 60); err != nil {
		t.Fatal(err)
	}
	if v := mr.HGet(signals.HourKey(now0.Add(-2*time.Hour)), "5"); v != "2" {
		t.Fatalf("小时桶=%q want 2", v)
	}
}
