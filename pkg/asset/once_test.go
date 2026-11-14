package asset

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

var qSeq = regexp.QuoteMeta("SELECT properties FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE")

func mockWriter(t *testing.T) (*Writer, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewWriter(sqlx.NewSqlConnFromDB(db)), mock
}

func set3(src string) []Asset {
	var out []Asset
	for _, u := range []string{"https://cdn/0", "https://cdn/1", "https://cdn/2"} {
		out = append(out, Asset{UserID: 7, SourceTaskID: src, Type: "image", ResultURL: u, Properties: `{"k":"v"}`})
	}
	return out
}

// 只补缺失的序号：已有 0、2 → 只写 1（properties 带 outputSeq=1，原有键保留）。
func TestInsertSetOnceIdxFillsMissingSeq(t *testing.T) {
	w, mock := mockWriter(t)
	mock.ExpectBegin()
	mock.ExpectQuery(qSeq).WithArgs("g1", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}).
		AddRow(`{"k":"v","outputSeq":0}`).AddRow(`{"outputSeq":2}`))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO afv_asset")).WithArgs(int64(7), int64(3), "g1", "image", "", "",
		nil, "https://cdn/1", "", `{"k":"v","outputSeq":1}`, nil, nil).WillReturnResult(sqlmock.NewResult(9, 1))
	mock.ExpectCommit()
	n, err := w.InsertSetOnceIdx(context.Background(), set3("g1"))
	if err != nil || n != 1 {
		t.Fatalf("应只补 1 行: n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 有不带序号的行（旧写法整组 / 叠字版派生行，含用户已删的）→ 视为整组已入库，什么都不写；全部序号齐 → 不写。
func TestInsertSetOnceIdxLegacyAndComplete(t *testing.T) {
	for name, rows := range map[string]*sqlmock.Rows{
		"legacy":   sqlmock.NewRows([]string{"properties"}).AddRow(`{"creationId":1}`),
		"null":     sqlmock.NewRows([]string{"properties"}).AddRow(nil),
		"complete": sqlmock.NewRows([]string{"properties"}).AddRow(`{"outputSeq":0}`).AddRow(`{"outputSeq":1}`).AddRow(`{"outputSeq":2}`),
	} {
		t.Run(name, func(t *testing.T) {
			w, mock := mockWriter(t)
			mock.ExpectBegin()
			mock.ExpectQuery(qSeq).WillReturnRows(rows)
			mock.ExpectCommit()
			if n, err := w.InsertSetOnceIdx(context.Background(), set3("g1")); err != nil || n != 0 {
				t.Fatalf("不应再写: n=%d err=%v", n, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 没有 source_task_id：拒绝（无从幂等）。
func TestInsertSetOnceIdxNoSource(t *testing.T) {
	w, mock := mockWriter(t)
	mock.ExpectBegin()
	mock.ExpectRollback()
	if _, err := w.InsertSetOnceIdx(context.Background(), set3("")); !errors.Is(err, ErrNoSource) {
		t.Fatalf("应拒绝: %v", err)
	}
}

// FinishWithAssets：置完成与入库同事务；入库失败 → 回滚后不带入库再置完成一次，AssetErr 带回原因。
func TestFinishWithAssetsFallback(t *testing.T) {
	w, mock := mockWriter(t)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE t SET status = 1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeq).WillReturnError(errors.New("lock wait timeout"))
	mock.ExpectRollback()
	mock.ExpectExec("UPDATE t SET status = 1").WillReturnResult(sqlmock.NewResult(0, 1))
	finish := func(ctx context.Context, s sqlx.Session) (bool, error) {
		r, err := s.ExecCtx(ctx, "UPDATE t SET status = 1 WHERE id = 1 AND status = 0")
		if err != nil {
			return false, err
		}
		n, _ := r.RowsAffected()
		return n > 0, nil
	}
	write := func(ctx context.Context, s sqlx.Session) error {
		_, err := InsertSetOnceTx(ctx, s, set3("g1"))
		return err
	}
	res, err := w.FinishWithAssets(context.Background(), finish, write)
	if err != nil || !res.OK || res.AssetErr == nil || !strings.Contains(res.AssetErr.Error(), "lock wait") {
		t.Fatalf("应退回只置完成: res=%+v err=%v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// CAS 没抢到：什么都不写；置完成本身出错：原样返回
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE t SET status = 1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if res, err := w.FinishWithAssets(context.Background(), finish, write); err != nil || res.OK {
		t.Fatalf("CAS 没抢到: %+v %v", res, err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE t SET status = 1").WillReturnError(errors.New("conn reset"))
	mock.ExpectRollback()
	if _, err := w.FinishWithAssets(context.Background(), finish, write); err == nil {
		t.Fatal("置完成出错应返回")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestKeysetAndRequeueCond(t *testing.T) {
	from, to := time.Unix(100, 0), time.Unix(200, 0)
	if c, a := KeysetCond("u", "id", from, to, ReconcileCursor{}); c != "u >= ? AND u < ?" || len(a) != 2 {
		t.Fatalf("首页条件: %s %v", c, a)
	}
	if c, a := KeysetCond("u", "id", from, to, ReconcileCursor{At: from, ID: 5}); c != "u < ? AND (u > ? OR (u = ? AND id > ?))" || len(a) != 4 || a[3] != int64(5) {
		t.Fatalf("续页条件: %s %v", c, a)
	}
	c, a := RequeueDueCond([]int{10, 20}, 3, 1000)
	if !strings.Contains(c, "CASE attempt DIV ? WHEN ? THEN ? WHEN ? THEN ? WHEN ? THEN ? ELSE ? END") {
		t.Fatalf("重投条件: %s", c)
	}
	if want := []any{int32(1000), 0, 10, 1, 20, 2, 20, 20}; len(a) != len(want) {
		t.Fatalf("参数: %v", a)
	}
	if RequeueBackoff(nil, 0) != 1800 || RequeueBackoff(nil, 99) != 43200 || RequeueBackoff([]int{0}, 0) != 1 {
		t.Fatal("退避取值不符")
	}
}

// 对账器：分页、跳过已有行 / Skip、无来源告警、补入失败计数、间隔门控。
func TestReconcilerRunOnce(t *testing.T) {
	w, mock := mockWriter(t)
	now := time.Unix(1_000_000, 0)
	pages := [][]ReconcileItem{
		{{ID: 1, At: now.Add(-3 * time.Hour), UserID: 7, SourceTaskID: "a"}, {ID: 2, At: now.Add(-2 * time.Hour), UserID: 7, SourceTaskID: "b"}},
		{{ID: 3, At: now.Add(-time.Hour), UserID: 7, SourceTaskID: ""}, {ID: 4, At: now.Add(-time.Hour), UserID: 7, SourceTaskID: "d", Skip: true}},
		{{ID: 5, At: now.Add(-time.Hour), UserID: 7, SourceTaskID: "e"}},
	}
	var cursors []ReconcileCursor
	var fixed []int64
	r := &Reconciler{Service: "t", Conf: ReconcileConf{Enabled: true, IntervalSeconds: 600, WindowHours: 168, GraceSeconds: 600, Batch: 2}, Writer: w,
		Now: func() time.Time { return now },
		List: func(_ context.Context, from, to time.Time, after ReconcileCursor, limit int) ([]ReconcileItem, error) {
			if !from.Equal(now.Add(-168*time.Hour)) || !to.Equal(now.Add(-600*time.Second)) || limit != 2 {
				t.Fatalf("窗口不符: %v %v %d", from, to, limit)
			}
			cursors = append(cursors, after)
			p := pages[0]
			pages = pages[1:]
			return p, nil
		},
		Fix: func(_ context.Context, it ReconcileItem) (bool, error) {
			fixed = append(fixed, it.ID)
			if it.ID == 5 {
				return false, errors.New("boom")
			}
			return true, nil
		},
	}
	q := regexp.QuoteMeta("SELECT DISTINCT source_task_id FROM afv_asset WHERE source_task_id IN")
	mock.ExpectQuery(q).WithArgs("a", "b").WillReturnRows(sqlmock.NewRows([]string{"source_task_id"}).AddRow("b"))
	// 第二页只有空来源与 Skip：没有可查的 id，不查库
	mock.ExpectQuery(q).WithArgs("e").WillReturnRows(sqlmock.NewRows([]string{"source_task_id"}))
	r.Tick(context.Background())
	r.Tick(context.Background()) // 不到间隔：不再扫
	if len(fixed) != 2 || fixed[0] != 1 || fixed[1] != 5 {
		t.Fatalf("应只补 a、e: %v", fixed)
	}
	if len(cursors) != 3 || cursors[1].ID != 2 || cursors[2].ID != 4 {
		t.Fatalf("游标不符: %+v", cursors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
