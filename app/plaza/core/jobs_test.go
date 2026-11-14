package core

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// H2：上翻只针对定时上线行（online_at IS NOT NULL），下翻不限。
func TestSyncLifecycleUpFlipOnlyScheduledRows(t *testing.T) {
	s, mock := sqlmockService(t)
	s.cfg.AdminWriteEnabled = true
	mock.ExpectExec(regexp.QuoteMeta("WHERE ((enabled = 1 AND " + enabledExpr + " = 0) OR (enabled = 0 AND online_at IS NOT NULL AND " + enabledExpr + " = 1))")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if n, err := s.SyncLifecycle(ctx0); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLifecycleNoopWhileJavaOwnsWrites(t *testing.T) {
	s, mock := sqlmockService(t) // AdminWriteEnabled=false
	if n, err := s.SyncLifecycle(ctx0); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// L13：生产（Mode=pro）沿用默认盐 / 空盐拒绝启动；其它模式放行。
func TestCheckIPHashSalt(t *testing.T) {
	for _, c := range []struct {
		mode, salt string
		bad        bool
	}{
		{"pro", DefaultIPHashSalt, true}, {"pro", "  ", true}, {"pro", "r4nd0m-s4lt", false},
		{"dev", DefaultIPHashSalt, false}, {"test", "", false},
	} {
		cfg := Config{IPHashSalt: c.salt}
		if err := cfg.CheckIPHashSalt(c.mode); (err != nil) != c.bad {
			t.Errorf("mode=%s salt=%q: err=%v want bad=%v", c.mode, c.salt, err, c.bad)
		}
	}
}
