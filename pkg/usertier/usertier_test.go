package usertier

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type fakeDB struct {
	paid      map[int64]bool
	paidErr   error
	paidCalls int
	knobs     map[string]string
	knobErr   error
	knobCalls int
}

func (f *fakeDB) resolver(d Defaults) *Resolver {
	return NewWith(func(_ context.Context, uid int64) (bool, error) {
		f.paidCalls++
		return f.paid[uid], f.paidErr
	}, func(_ context.Context, key string) (string, bool, error) {
		f.knobCalls++
		if f.knobErr != nil {
			return "", false, f.knobErr
		}
		v, ok := f.knobs[key]
		return v, ok, nil
	}, d)
}

var on = Defaults{UserTierEnabled: true, DisplayWatermarkEnabled: true}

func TestFreeAndPaid(t *testing.T) {
	db := &fakeDB{paid: map[int64]bool{2: true}}
	r := db.resolver(on)
	ctx := context.Background()
	if !r.IsFree(ctx, 1) || !r.WatermarkedImage(ctx, 1) {
		t.Fatal("没付过钱应为免费档并带水印")
	}
	if r.IsFree(ctx, 2) || r.WatermarkedImage(ctx, 2) {
		t.Fatal("付过钱应为付费档")
	}
	if r.IsFree(ctx, 0) || r.IsFree(ctx, -1) {
		t.Fatal("非法用户按付费")
	}
	var nilR *Resolver
	if nilR.IsFree(ctx, 1) || nilR.WatermarkedImage(ctx, 1) || nilR.HasEverPaid(ctx, 1) {
		t.Fatal("nil resolver 按付费、未付过")
	}
}

// 只缓存付费：免费每次查库（刚充值立刻生效），付费 TTL 内不再查。
func TestCachesOnlyPaid(t *testing.T) {
	db := &fakeDB{paid: map[int64]bool{}}
	r := db.resolver(on)
	now := time.Unix(1000, 0)
	r.SetClock(func() time.Time { return now })
	ctx := context.Background()
	r.HasEverPaid(ctx, 1)
	r.HasEverPaid(ctx, 1)
	if db.paidCalls != 2 {
		t.Fatalf("免费不应缓存: calls=%d", db.paidCalls)
	}
	db.paid[1] = true // 充值
	if !r.HasEverPaid(ctx, 1) {
		t.Fatal("充值后立刻按付费")
	}
	r.HasEverPaid(ctx, 1)
	if db.paidCalls != 3 {
		t.Fatalf("付费应缓存: calls=%d", db.paidCalls)
	}
	now = now.Add(DefaultPaidTTL + time.Second)
	r.HasEverPaid(ctx, 1)
	if db.paidCalls != 4 {
		t.Fatalf("过期后重查: calls=%d", db.paidCalls)
	}
}

// 查询失败按付费处理且不缓存。
func TestLookupFailureTreatedAsPaid(t *testing.T) {
	db := &fakeDB{paidErr: errors.New("db down")}
	r := db.resolver(on)
	ctx := context.Background()
	if r.IsFree(ctx, 1) || !r.HasEverPaid(ctx, 1) {
		t.Fatal("查询失败应按付费")
	}
	db.paidErr = nil
	if !r.IsFree(ctx, 1) {
		t.Fatal("失败不缓存：恢复后按真实结果")
	}
}

// 开关：DB > 默认；非 "true" 即关；关掉分层所有人付费；关掉展示水印不带水印但 IsFree 不变。
func TestKnobs(t *testing.T) {
	ctx := context.Background()
	db := &fakeDB{knobs: map[string]string{KeyUserTierEnabled: "false"}}
	if db.resolver(on).IsFree(ctx, 1) {
		t.Fatal("分层关 → 付费")
	}
	db = &fakeDB{knobs: map[string]string{KeyUserTierEnabled: "TRUE"}}
	if !db.resolver(Defaults{}).IsFree(ctx, 1) {
		t.Fatal("DB 开优先于默认关")
	}
	db = &fakeDB{knobs: map[string]string{KeyUserTierEnabled: "yes"}}
	if db.resolver(on).IsFree(ctx, 1) {
		t.Fatal("非 true 视为关（Boolean.parseBoolean）")
	}
	db = &fakeDB{knobs: map[string]string{KeyUserTierEnabled: ""}}
	if !db.resolver(on).IsFree(ctx, 1) {
		t.Fatal("空值回落默认")
	}
	db = &fakeDB{knobs: map[string]string{KeyDisplayWatermarkEnabled: "false"}}
	r := db.resolver(on)
	if r.WatermarkedImage(ctx, 1) || !r.IsFree(ctx, 1) {
		t.Fatal("展示水印关：不带水印，但仍是免费档")
	}
}

// 开关缓存 60 秒；读失败沿用上次值。
func TestKnobCacheAndFailure(t *testing.T) {
	ctx := context.Background()
	db := &fakeDB{knobs: map[string]string{KeyUserTierEnabled: "false"}}
	r := db.resolver(on)
	now := time.Unix(1000, 0)
	r.SetClock(func() time.Time { return now })
	r.TierEnabled(ctx)
	r.TierEnabled(ctx)
	if db.knobCalls != 1 {
		t.Fatalf("应缓存: %d", db.knobCalls)
	}
	now = now.Add(61 * time.Second)
	db.knobErr = errors.New("blip")
	if r.TierEnabled(ctx) {
		t.Fatal("失败沿用上次的 false")
	}
}

// SQL 与 Java 判据一致：付费来源、逻辑删除。
func TestSQLMatchesRule(t *testing.T) {
	for _, s := range PaidSources {
		if !strings.Contains(PaidExistsSQL, "'"+s+"'") || !strings.Contains(PaidExistsCond("t.user_id"), "'"+s+"'") {
			t.Fatalf("缺少付费来源 %s", s)
		}
	}
	for _, s := range []string{"REGISTER_GIFT", "CAMPAIGN_GIFT", "ADMIN_GIFT", "ADMIN_ADJUST", "REFUND"} {
		if strings.Contains(PaidExistsSQL, s) {
			t.Fatalf("%s 不是付费来源", s)
		}
	}
	if !strings.Contains(PaidExistsSQL, "deleted = 0") || !strings.Contains(PaidExistsCond("x"), "p.user_id = x") {
		t.Fatal("判据不完整")
	}
}

// 真实连接路径（sqlmock）：付费查询 + 开关查询。
func TestNewWithConn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := sqlx.NewSqlConnFromDB(db)
	r := New(conn, on)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_system_config WHERE config_key = ?")).WithArgs(KeyUserTierEnabled).
		WillReturnRows(sqlmock.NewRows([]string{"v"}))
	mock.ExpectQuery(regexp.QuoteMeta(PaidExistsSQL)).WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	if !r.IsFree(context.Background(), 7) {
		t.Fatal("没付费批次 → 免费")
	}
	mock.ExpectQuery(regexp.QuoteMeta(PaidExistsSQL)).WithArgs(int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	if r.IsFree(context.Background(), 8) {
		t.Fatal("有付费批次 → 付费")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
