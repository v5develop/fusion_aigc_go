package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func newSwitchMock(t *testing.T, configDefault bool) (*Switch, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewSwitch(sqlx.NewSqlConnFromDB(db), configDefault)
	s.now = func() time.Time { return time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC) }
	return s, mock
}

const qSwitchGet = `SELECT .* FROM afv_system_config WHERE config_key = \? AND deleted = 0`

func switchRows(v string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"config_value", "remark", "update_time"}).AddRow(v, "r", time.Now())
}

// 数据库 > 文件配置：库里没有 / 值非法跟随部署配置，合法值以库为准
func TestSwitchGetSource(t *testing.T) {
	s, mock := newSwitchMock(t, true)
	mock.ExpectQuery(qSwitchGet).WithArgs(KeyEnabled).WillReturnRows(sqlmock.NewRows([]string{"config_value", "remark", "update_time"}))
	v, err := s.Get(context.Background())
	if err != nil || v.Source != SwitchSourceConfig || !v.Enabled || v.DBValue != nil || v.EffectSeconds != 60 {
		t.Fatalf("没配应跟随部署配置: %+v %v", v, err)
	}
	mock.ExpectQuery(qSwitchGet).WithArgs(KeyEnabled).WillReturnRows(switchRows("false"))
	if v, _ = s.Get(context.Background()); v.Source != SwitchSourceDB || v.Enabled {
		t.Fatalf("库值 false 应以库为准: %+v", v)
	}
	mock.ExpectQuery(qSwitchGet).WithArgs(KeyEnabled).WillReturnRows(switchRows("maybe"))
	if v, _ = s.Get(context.Background()); v.Source != SwitchSourceConfig || !v.Enabled || v.DBValue == nil || *v.DBValue != "maybe" {
		t.Fatalf("非法值应跟随部署配置并带原值: %+v", v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchSetAndReset(t *testing.T) {
	s, mock := newSwitchMock(t, false)
	if _, err := s.Set(context.Background(), SwitchReq{AdminID: 9, Enabled: true, Reason: "  "}); err == nil {
		t.Fatal("原因必填")
	}
	if _, err := s.Set(context.Background(), SwitchReq{AdminID: 9, Enabled: true, Reason: strings.Repeat("长", 201)}); err == nil {
		t.Fatal("原因超长应拒收")
	}
	mock.ExpectExec(`INSERT INTO afv_system_config .* ON DUPLICATE KEY UPDATE .*deleted = 0`).
		WithArgs(KeyEnabled, "true", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(qSwitchGet).WithArgs(KeyEnabled).WillReturnRows(switchRows("true"))
	v, err := s.Set(context.Background(), SwitchReq{AdminID: 9, Enabled: true, Reason: "上线"})
	if err != nil || !v.Enabled || v.Source != SwitchSourceDB {
		t.Fatalf("set: %+v %v", v, err)
	}
	mock.ExpectExec(`UPDATE afv_system_config SET deleted = 1, remark = \? WHERE config_key = \? AND deleted = 0`).
		WithArgs(sqlmock.AnyArg(), KeyEnabled).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSwitchGet).WithArgs(KeyEnabled).WillReturnRows(sqlmock.NewRows([]string{"config_value", "remark", "update_time"}))
	v, err = s.Reset(context.Background(), 9, "回到部署配置")
	if err != nil || v.Enabled || v.Source != SwitchSourceConfig {
		t.Fatalf("reset: %+v %v", v, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchRemark(t *testing.T) {
	s, _ := newSwitchMock(t, false)
	got := s.remark(9, "设为暂停新提交", "排查网关")
	if got != "视频做同款总开关：管理员 9 于 2026-11-16 11:00 设为暂停新提交。原因：排查网关" {
		t.Fatalf("remark = %q", got)
	}
	if r := []rune(s.remark(9, "x", strings.Repeat("长", 300))); len(r) != 256 {
		t.Fatalf("remark 应截到 256 字符，得 %d", len(r))
	}
}
