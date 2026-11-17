package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	radmin "aigc-local-life/app/replicate/admin"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var cfgDefault = radmin.RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 3}

func newRewardMock(t *testing.T, cfg radmin.RewardSettings) (*ShareRewardSettings, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewShareRewardSettings(sqlx.NewSqlConnFromDB(db), cfg)
	s.now = func() time.Time { return time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC) }
	return s, mock
}

const qRewardGet = `SELECT config_key, .* FROM afv_system_config WHERE config_key IN \(\?, \?, \?\) AND deleted = 0`

func rewardRows(kv ...string) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"config_key", "config_value", "remark", "update_time"})
	for i := 0; i+1 < len(kv); i += 2 {
		r.AddRow(kv[i], kv[i+1], "rk", time.Date(2026, 11, 17, 1, 0, i, 0, time.UTC))
	}
	return r
}

func expectGet(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(qRewardGet).WithArgs(KeyShareRewardEnabled, KeyShareRewardPoints, KeyShareRewardMonthlyLimit).WillReturnRows(rows)
}

// 库里没有：全部跟随部署配置
func TestShareRewardFallbackToConfig(t *testing.T) {
	s, mock := newRewardMock(t, radmin.RewardSettings{Enabled: false, Points: 20, MonthlyLimit: 0})
	expectGet(mock, rewardRows())
	v, err := s.Get(context.Background())
	if err != nil || v.Source != SwitchSourceConfig || v.Enabled || v.Points != 20 || v.MonthlyLimit != 0 || v.ConfigDefault.Points != 20 || v.UpdatedAt != nil {
		t.Fatalf("%+v %v", v, err)
	}
}

// 逐项判定：库里合法项覆盖，非法项回落配置
func TestShareRewardPerKeyValidationAndFallback(t *testing.T) {
	s, mock := newRewardMock(t, cfgDefault)
	expectGet(mock, rewardRows(KeyShareRewardEnabled, "false", KeyShareRewardPoints, "5000", KeyShareRewardMonthlyLimit, "0"))
	v, err := s.Get(context.Background())
	if err != nil || v.Source != SwitchSourceDB || v.Enabled || v.Points != 30 || v.MonthlyLimit != 0 {
		t.Fatalf("点数越界应回落 30，其余取库：%+v %v", v, err)
	}
	for _, c := range []struct {
		name string
		rows *sqlmock.Rows
		want radmin.RewardSettings
	}{
		{"全非法", rewardRows(KeyShareRewardEnabled, "maybe", KeyShareRewardPoints, "abc", KeyShareRewardMonthlyLimit, "-1"), cfgDefault},
		{"上限 101", rewardRows(KeyShareRewardMonthlyLimit, "101"), cfgDefault},
		{"点数 0", rewardRows(KeyShareRewardPoints, "0"), cfgDefault},
		{"合法 0 不限", rewardRows(KeyShareRewardMonthlyLimit, " 0 "), radmin.RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 0}},
		{"全合法", rewardRows(KeyShareRewardEnabled, "true", KeyShareRewardPoints, "1000", KeyShareRewardMonthlyLimit, "100"), radmin.RewardSettings{Enabled: true, Points: 1000, MonthlyLimit: 100}},
	} {
		expectGet(mock, c.rows)
		if got := s.Load(context.Background()); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	// 全非法时 source 仍是 config
	expectGet(mock, rewardRows(KeyShareRewardPoints, "abc"))
	if v, _ := s.Get(context.Background()); v.Source != SwitchSourceConfig {
		t.Fatalf("全非法 source 应为 config：%+v", v)
	}
}

// 读库失败：Load 回落部署配置（不 panic、不放大故障）；Get 如实报错
func TestShareRewardLoadDBError(t *testing.T) {
	s, mock := newRewardMock(t, cfgDefault)
	mock.ExpectQuery(qRewardGet).WillReturnError(context.DeadlineExceeded)
	if got := s.Load(context.Background()); got != cfgDefault {
		t.Fatalf("读库失败应回落：%+v", got)
	}
	mock.ExpectQuery(qRewardGet).WillReturnError(context.DeadlineExceeded)
	if _, err := s.Get(context.Background()); err == nil {
		t.Fatal("Get 读库失败应报错")
	}
}

func TestShareRewardNonPositiveConfigFallsBackToBuiltin(t *testing.T) {
	s, _ := newRewardMock(t, radmin.RewardSettings{Enabled: true, Points: 0, MonthlyLimit: 500})
	if s.configDefault != radmin.DefaultRewardSettings() {
		t.Fatalf("非法 yaml 应退内置默认：%+v", s.configDefault)
	}
	if NewShareRewardSettings(nil, cfgDefault) != nil {
		t.Fatal("conn 为 nil 应返回 nil")
	}
}

func TestShareRewardSetValidation(t *testing.T) {
	s, _ := newRewardMock(t, cfgDefault)
	ctx := context.Background()
	for _, r := range []ShareRewardReq{
		{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 3}, Reason: " "},
		{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 3}, Reason: strings.Repeat("长", 201)},
		{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: true, Points: 0, MonthlyLimit: 3}, Reason: "x"},
		{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: true, Points: 1001, MonthlyLimit: 3}, Reason: "x"},
		{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 101}, Reason: "x"},
		{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: true, Points: 30, MonthlyLimit: -1}, Reason: "x"},
	} {
		if _, err := s.Set(ctx, r); err == nil {
			t.Errorf("%+v 应拒收", r)
		}
	}
	if _, err := s.Reset(ctx, 9, ""); xerr.ReasonOf(err) == "" && err == nil {
		t.Error("恢复默认原因必填")
	}
}

func TestShareRewardSetAndReset(t *testing.T) {
	s, mock := newRewardMock(t, cfgDefault)
	ctx := context.Background()
	mock.ExpectBegin()
	for _, kv := range [][2]string{{KeyShareRewardEnabled, "false"}, {KeyShareRewardPoints, "50"}, {KeyShareRewardMonthlyLimit, "0"}} {
		mock.ExpectExec(`INSERT INTO afv_system_config .* ON DUPLICATE KEY UPDATE .*deleted = 0`).
			WithArgs(kv[0], kv[1], sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
	expectGet(mock, rewardRows(KeyShareRewardEnabled, "false", KeyShareRewardPoints, "50", KeyShareRewardMonthlyLimit, "0"))
	v, err := s.Set(ctx, ShareRewardReq{AdminID: 9, RewardSettings: radmin.RewardSettings{Enabled: false, Points: 50, MonthlyLimit: 0}, Reason: "活动结束"})
	if err != nil || v.Source != SwitchSourceDB || v.Enabled || v.Points != 50 || v.MonthlyLimit != 0 || v.Remark == "" {
		t.Fatalf("set: %+v %v", v, err)
	}
	mock.ExpectExec(`UPDATE afv_system_config SET deleted = 1, remark = \? WHERE config_key IN \(\?, \?, \?\) AND deleted = 0`).
		WithArgs(sqlmock.AnyArg(), KeyShareRewardEnabled, KeyShareRewardPoints, KeyShareRewardMonthlyLimit).WillReturnResult(sqlmock.NewResult(0, 3))
	expectGet(mock, rewardRows())
	v, err = s.Reset(ctx, 9, "回到部署配置")
	if err != nil || v.Source != SwitchSourceConfig || !v.Enabled || v.Points != 30 {
		t.Fatalf("reset: %+v %v", v, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestShareRewardRemark(t *testing.T) {
	s, _ := newRewardMock(t, cfgDefault)
	if got := s.remark(9, "设为开启、每条 30 点、每人每月 3 次", "活动"); got != "分享奖励规则：管理员 9 于 2026-11-17 11:00 设为开启、每条 30 点、每人每月 3 次。原因：活动" {
		t.Fatalf("remark = %q", got)
	}
	if r := []rune(s.remark(9, "x", strings.Repeat("长", 300))); len(r) != 256 {
		t.Fatalf("remark 应截到 256 字符，得 %d", len(r))
	}
}
