package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newSwitchForTest(t *testing.T, yaml bool) (*IndustrySwitch, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w := newIndustrySwitch(db, yaml)
	w.now = func() time.Time { return time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC) }
	return w, mock
}

const qIndSwitch = `SELECT config_value, remark, update_time FROM afv_system_config WHERE config_key = \? AND deleted = 0 LIMIT 1`

func indRow(v string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"config_value", "remark", "update_time"}).AddRow(v, "r", time.Now())
}

// 数据库 > 配置文件：库里合法值以库为准；没有 / 非法 / 读库失败用配置文件
func TestIndustrySwitchPrecedence(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		yaml bool
		rows *sqlmock.Rows
		err  error
		want bool
	}{
		{"库开配置关", false, indRow("true"), nil, true},
		{"库关配置开", true, indRow("false"), nil, false},
		{"库没有用配置开", true, sqlmock.NewRows([]string{"config_value", "remark", "update_time"}), nil, true},
		{"库非法用配置关", false, indRow("maybe"), nil, false},
		{"读库失败用配置开", true, nil, errors.New("db down"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, mock := newSwitchForTest(t, c.yaml)
			q := mock.ExpectQuery(qIndSwitch).WithArgs(IndustrySwitchKey)
			if c.err != nil {
				q.WillReturnError(c.err)
			} else {
				q.WillReturnRows(c.rows)
			}
			if got := w.Enabled(ctx); got != c.want {
				t.Fatalf("enabled=%v want %v", got, c.want)
			}
		})
	}
}

// 60 秒内走缓存，不重复查库
func TestIndustrySwitchCaches(t *testing.T) {
	w, mock := newSwitchForTest(t, false)
	mock.ExpectQuery(qIndSwitch).WithArgs(IndustrySwitchKey).WillReturnRows(indRow("true"))
	_ = w.Enabled(context.Background())
	_ = w.Enabled(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestIndustryReasonRequired(t *testing.T) {
	if _, err := industrySwitchReason("  "); err == nil {
		t.Fatal("原因必填")
	}
}
