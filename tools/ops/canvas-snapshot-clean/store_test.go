package main

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	snapDirty = `{"nodes":[{"id":"n","type":"model","data":{"runId":"r2","run_id":"r1","seed":12345678901234567890}}],"edges":[]}`
	snapClean = `{"nodes":[{"id":"x","type":"text","data":{"text":"a"}}],"edges":[]}`
	fixedSnap = `{"edges":[],"nodes":[{"data":{"runId":"r2","seed":12345678901234567890},"id":"n","type":"model"}]}`
)

var selCols = []string{"id", "revision", "updated_at", "snapshot"}

func newMock(t *testing.T) (*store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &store{db: db}, mock
}

var (
	qCanvas   = regexp.QuoteMeta("SELECT `id`, `revision`, DATE_FORMAT(`updated_at`, '%Y-%m-%d %H:%i:%s.%f'), CAST(`snapshot` AS CHAR) FROM `afv_canvas` WHERE `id` > ? AND `deleted` = 0")
	qTemplate = regexp.QuoteMeta("SELECT `id`, '', DATE_FORMAT(`updated_at`, '%Y-%m-%d %H:%i:%s.%f'), CAST(`snapshot` AS CHAR) FROM `afv_canvas_template` WHERE `id` > ? AND `deleted` = 0")
	uCanvas   = regexp.QuoteMeta("UPDATE `afv_canvas` SET `snapshot` = CAST(? AS JSON), `updated_at` = `updated_at` WHERE `id` = ? AND `revision` = ? AND `updated_at` = ?")
	uTemplate = regexp.QuoteMeta("UPDATE `afv_canvas_template` SET `snapshot` = CAST(? AS JSON), `updated_at` = `updated_at` WHERE `id` = ? AND `updated_at` = ?")
)

func TestDryRunKeysetPaginationNoWrites(t *testing.T) {
	st, mock := newMock(t)
	// 第一批满 2 行 → 继续从最后一个 id 取；第二批不满 → 结束
	mock.ExpectQuery(qCanvas+".*ORDER BY `id` LIMIT \\?").WithArgs("", 2).
		WillReturnRows(sqlmock.NewRows(selCols).
			AddRow("cv_a", "1-aaaa", "2026-11-01 10:00:00.123000", snapDirty).
			AddRow("cv_b", "1-bbbb", "2026-11-01 10:00:00.000000", snapClean))
	mock.ExpectQuery(qCanvas).WithArgs("cv_b", 2).
		WillReturnRows(sqlmock.NewRows(selCols).AddRow("cv_c", "1-cccc", "2026-11-02 00:00:00.000000", snapDirty))
	mock.ExpectQuery(qTemplate).WithArgs(int64(0), 2).WillReturnRows(sqlmock.NewRows(selCols))

	var out bytes.Buffer
	s, err := run(context.Background(), st, options{Targets: targets, Scan: scanOpts{Batch: 2}}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if s.Scanned != 3 || s.Polluted != 2 || s.KeysRemoved != 2 || s.KeysDiffered != 2 || s.ByResult[resWould] != 2 || s.ByResult[resClean] != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if !strings.Contains(out.String(), "ROW table=afv_canvas id=cv_a removed=1 differed=1 result=would_fix") ||
		!strings.Contains(out.String(), "$.nodes[0].data.run_id") {
		t.Errorf("report:\n%s", out.String())
	}
}

func TestApplyBacksUpThenWritesWithOptimisticLock(t *testing.T) {
	st, mock := newMock(t)
	mock.ExpectQuery(qCanvas+regexp.QuoteMeta(" AND `updated_at` >= ?")).WithArgs("", "2026-10-01 00:00:00", 10).
		WillReturnRows(sqlmock.NewRows(selCols).
			AddRow("cv_a", "1-aaaa", "2026-11-01 10:00:00.123000", snapDirty).
			AddRow("cv_b", "1-bbbb", "2026-11-01 11:00:00.000000", snapDirty))
	mock.ExpectExec(uCanvas).WithArgs(fixedSnap, "cv_a", "1-aaaa", "2026-11-01 10:00:00.123000").WillReturnResult(sqlmock.NewResult(0, 1))
	// cv_b 扫描后被用户保存：revision 已变 → 0 行 → conflict
	mock.ExpectExec(uCanvas).WithArgs(fixedSnap, "cv_b", "1-bbbb", "2026-11-01 11:00:00.000000").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(qTemplate+regexp.QuoteMeta(" AND `updated_at` >= ?")).WithArgs(int64(0), "2026-10-01 00:00:00", 10).
		WillReturnRows(sqlmock.NewRows(selCols).AddRow("7", "", "2026-11-03 09:00:00.500000", snapDirty))
	mock.ExpectExec(uTemplate).WithArgs(fixedSnap, int64(7), "2026-11-03 09:00:00.500000").WillReturnResult(sqlmock.NewResult(0, 1))

	var backup bytes.Buffer
	synced := 0
	var out bytes.Buffer
	o := options{Targets: targets, Scan: scanOpts{Batch: 10, Since: "2026-10-01 00:00:00"}, Apply: true,
		Backup: &backup, Sync: func() error { synced++; return nil }}
	s, err := run(context.Background(), st, o, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if s.ByResult[resFixed] != 2 || s.ByResult[resConflict] != 1 {
		t.Fatalf("summary = %+v\n%s", s, out.String())
	}
	if synced != 2 {
		t.Errorf("backup synced %d times, want once per batch with fixes (2)", synced)
	}
	lines := strings.Split(strings.TrimSpace(backup.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("backup lines = %d:\n%s", len(lines), backup.String())
	}
	var bl backupLine
	if err := json.Unmarshal([]byte(lines[0]), &bl); err != nil {
		t.Fatal(err)
	}
	if bl.Table != "afv_canvas" || bl.ID != "cv_a" || bl.Revision != "1-aaaa" || string(bl.Snapshot) != snapDirty {
		t.Errorf("backup line = %+v", bl)
	}
	if !strings.Contains(out.String(), "id=cv_b removed=1 differed=1 result=conflict") {
		t.Errorf("conflict not reported:\n%s", out.String())
	}
}

type anyRevision struct{}

func (anyRevision) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && regexp.MustCompile(`^\d{13}-[0-9a-f]{4}$`).MatchString(s)
}

func TestApplyBumpRevisionAndLimit(t *testing.T) {
	st, mock := newMock(t)
	mock.ExpectQuery(qCanvas).WithArgs("", 1).
		WillReturnRows(sqlmock.NewRows(selCols).AddRow("cv_a", "1-aaaa", "2026-11-01 10:00:00.123000", snapDirty))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas` SET `snapshot` = CAST(? AS JSON), `updated_at` = `updated_at`, `revision` = ? WHERE `id` = ? AND `revision` = ? AND `updated_at` = ?")).
		WithArgs(fixedSnap, anyRevision{}, "cv_a", "1-aaaa", "2026-11-01 10:00:00.123000").WillReturnResult(sqlmock.NewResult(0, 1))

	var backup, out bytes.Buffer
	o := options{Targets: targets[:1], Scan: scanOpts{Batch: 50}, Limit: 1, Apply: true, BumpRevision: true, Backup: &backup}
	s, err := run(context.Background(), st, o, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if s.Scanned != 1 || s.ByResult[resFixed] != 1 {
		t.Fatalf("summary = %+v", s)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestApplyBackupFailureSkipsWrites(t *testing.T) {
	st, mock := newMock(t)
	mock.ExpectQuery(qCanvas).WithArgs("", 50).
		WillReturnRows(sqlmock.NewRows(selCols).AddRow("cv_a", "1-aaaa", "2026-11-01 10:00:00.123000", snapDirty))
	// 没有 ExpectExec：一旦写库 sqlmock 会报错

	var out bytes.Buffer
	o := options{Targets: targets, Scan: scanOpts{Batch: 50}, Apply: true, Backup: failWriter{}}
	s, err := run(context.Background(), st, o, &out)
	if err == nil || !strings.Contains(err.Error(), "写备份失败") {
		t.Fatalf("backup failure must abort the run, err=%v", err) // 也不再去扫模板表（没有对应 ExpectQuery）
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if s.ByResult[resError] != 1 || s.ByResult[resFixed] != 0 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestBadJSONRowReportedNotWritten(t *testing.T) {
	st, mock := newMock(t)
	mock.ExpectQuery(qCanvas).WithArgs("", 50).
		WillReturnRows(sqlmock.NewRows(selCols).AddRow("cv_a", "1-aaaa", "2026-11-01 10:00:00.123000", `{"nodes":`))
	var backup, out bytes.Buffer
	s, err := run(context.Background(), st, options{Targets: targets[:1], Scan: scanOpts{Batch: 50}, Apply: true, Backup: &backup}, &out)
	if err != nil || s.ByResult[resBadJSON] != 1 || backup.Len() != 0 {
		t.Fatalf("summary = %+v err=%v backup=%q", s, err, backup.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPickTargets(t *testing.T) {
	if ts, err := pickTargets("template, canvas,template"); err != nil || len(ts) != 2 || ts[0].Table != "afv_canvas_template" {
		t.Fatalf("pickTargets = %+v %v", ts, err)
	}
	if _, err := pickTargets("canvas,run"); err == nil {
		t.Fatal("unknown table accepted")
	}
}

func TestNormalizeDSNForcesUTF8MB4(t *testing.T) {
	for _, dsn := range []string{
		"u:p@tcp(h:3306)/db",
		"u:p@tcp(h:3306)/db?charset=latin1",
		"u:p@tcp(h:3306)/db?charset=utf8&collation=utf8_general_ci",
	} {
		cfg, err := normalizeDSN(dsn)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		got := cfg.FormatDSN()
		if !strings.Contains(got, "charset=utf8mb4") || cfg.Collation != "utf8mb4_general_ci" || strings.Contains(got, "latin1") {
			t.Errorf("%s → %s (collation %s)", dsn, got, cfg.Collation)
		}
	}
	if _, err := normalizeDSN("u:secretpw@tcp(h:3306)db"); err == nil || strings.Contains(err.Error(), "secretpw") {
		t.Errorf("bad DSN error must not echo the password: %v", err)
	}
}

func TestOpenBackupRefusesExisting(t *testing.T) {
	p := t.TempDir() + "/b.jsonl"
	f, err := openBackup(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := openBackup(p); err == nil {
		t.Fatal("existing backup file must not be reopened / overwritten")
	}
}
