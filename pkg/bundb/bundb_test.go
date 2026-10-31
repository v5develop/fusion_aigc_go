package bundb

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

type note struct {
	bun.BaseModel `bun:"table:afv_studio_note"`
	ID            int64  `bun:"id,pk"`
	Status        int    `bun:"status"`
	Title         string `bun:"title"`
}

func newMock(t *testing.T) (*bun.DB, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	// 方言首次使用会探测服务端版本，sqlmock 下失败只打一行日志，不影响断言
	return bun.NewDB(sdb, mysqldialect.New(mysqldialect.WithTimeLocation("UTC"))), mock
}

func TestUpdateIf(t *testing.T) {
	for _, tc := range []struct {
		affected int64
		want     bool
	}{{0, false}, {1, true}} {
		db, mock := newMock(t)
		mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_note`") + ".*SET `title` = .x. WHERE \\(id = 7\\) AND \\(status = 1\\)").
			WillReturnResult(sqlmock.NewResult(0, tc.affected))
		ok, err := UpdateIf(context.Background(), db, (*note)(nil), map[string]any{"title": "x"},
			func(q *bun.UpdateQuery) *bun.UpdateQuery { return q.Where("id = ?", 7).Where("status = ?", 1) })
		if err != nil || ok != tc.want {
			t.Fatalf("affected=%d: ok=%v err=%v", tc.affected, ok, err)
		}
	}
}

func TestUpdateIfValidation(t *testing.T) {
	db, _ := newMock(t)
	if _, err := UpdateIf(context.Background(), db, (*note)(nil), map[string]any{"a": 1}, nil); err == nil {
		t.Fatal("where 为空应报错")
	}
}

func TestTransitStatus(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectExec("UPDATE `afv_studio_note` SET `status` = 2, `updated_at` = NOW\\(3\\), `fail_reason` = 'boom' WHERE \\(`id` = 9\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := TransitStatus(context.Background(), db, "afv_studio_note", 9, 1, 2, map[string]any{"fail_reason": "boom"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsert(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectExec("INSERT INTO `afv_studio_note`.*ON DUPLICATE KEY UPDATE `title` = VALUES\\(`title`\\)").
		WillReturnResult(sqlmock.NewResult(1, 1))
	err := Upsert(context.Background(), db, &note{ID: 1, Status: 1, Title: "t"}, []string{"id"}, []string{"title"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(context.Background(), db, &note{}, []string{"id"}, []string{"id"}); err == nil {
		t.Fatal("updateCols 含冲突列应报错")
	}
}

func TestRunInTx(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectCommit()
	if err := RunInTx(context.Background(), db, func(context.Context, bun.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectRollback()
	boom := errors.New("boom")
	if err := RunInTx(context.Background(), db, func(context.Context, bun.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}

func TestHookSlowAndError(t *testing.T) {
	c := logtest.NewCollector(t)
	h := newHook(10*time.Millisecond, false)
	ev := func(d time.Duration, err error) *bun.QueryEvent {
		return &bun.QueryEvent{Query: "SELECT 1", StartTime: time.Now().Add(-d), Err: err}
	}
	ctx := context.Background()

	h.AfterQuery(ctx, ev(time.Second, nil))
	if got := c.Content(); !regexp.MustCompile("bun slow query").MatchString(got) {
		t.Fatalf("慢查询应记录, got %q", got)
	}
	c.Reset()
	h.AfterQuery(ctx, ev(0, sql.ErrNoRows))
	if got := c.Content(); got != "" {
		t.Fatalf("ErrNoRows 快查询不应有日志, got %q", got)
	}
	h.AfterQuery(ctx, ev(0, errors.New("dead")))
	if got := c.Content(); !regexp.MustCompile("bun query error").MatchString(got) {
		t.Fatalf("错误应记录, got %q", got)
	}
}

func TestTruncate(t *testing.T) {
	long := make([]rune, 1500)
	for i := range long {
		long[i] = '中'
	}
	if got := []rune(truncate(string(long), maxSQLLen)); len(got) != maxSQLLen+len([]rune("...(truncated)")) {
		t.Fatalf("len=%d", len(got))
	}
}
