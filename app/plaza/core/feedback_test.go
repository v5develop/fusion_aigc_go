package core

import (
	"regexp"
	"testing"
	"time"

	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

func TestCleanText(t *testing.T) {
	if got := cleanText("  美甲\t店\n\n 宠物\x00店  ", 40); got != "美甲 店 宠物店" {
		t.Fatalf("%q", got)
	}
	if got := cleanText("一二三四五", 3); got != "一二三" {
		t.Fatalf("按字符截断: %q", got)
	}
}

func TestLooksPersonal(t *testing.T) {
	for _, s := range []string{"13800138000", "138 0013 8000", "a@b.com", "我的邮箱 x.y@z.cn", "11010519491231002X"} {
		if !looksPersonal(s) {
			t.Errorf("%q 应判为疑似联系方式", s)
		}
	}
	for _, s := range []string{"美甲", "宠物店", "民宿 2024"} {
		if looksPersonal(s) {
			t.Errorf("%q 不应误判", s)
		}
	}
}

func TestNormText(t *testing.T) {
	if got := normText(" 美 甲，Shop! "); got != "美甲shop" {
		t.Fatal(got)
	}
	if normText("，。 ") != "" {
		t.Fatal("全是标点应归一为空")
	}
}

func TestCleanMissTerm(t *testing.T) {
	ok := map[string]string{" 按摩  套餐 ": "按摩 套餐", "AbC": "abc"}
	for in, want := range ok {
		if got := cleanMissTerm(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
	// 疑似手机号 / 邮箱 / 证件号、纯数字串一律丢弃
	for _, in := range []string{"13800138000", "a@b.com", "110105194912310021", "123456", "1 2 3", "", "   "} {
		if got := cleanMissTerm(in); got != "" {
			t.Errorf("%q 应丢弃，得到 %q", in, got)
		}
	}
	long := ""
	for i := 0; i < 50; i++ {
		long += "字"
	}
	if got := cleanMissTerm(long); len([]rune(got)) != 30 {
		t.Fatalf("截断到 30 字: %d", len([]rune(got)))
	}
}

func TestHashIPStable(t *testing.T) {
	s := newTestService(stdFixture())
	a, b := s.HashIP("1.2.3.4"), s.HashIP("1.2.3.4")
	if a != b || len(a) != 16 || a == s.HashIP("1.2.3.5") || s.HashIP("") != "" {
		t.Fatalf("%q %q", a, b)
	}
}

func sqlmockService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	s := New(bun.NewDB(sdb, mysqldialect.New()), nil, Config{})
	s.now = func() time.Time { return t0 }
	return s, mock
}

const sid = "abcdef0123456789abcdef0123456789"

func TestSubmitSuggestionValidationAndDrop(t *testing.T) {
	s, mock := sqlmockService(t)
	if err := s.SubmitSuggestion(ctx0, SuggestionInput{Text: "美甲"}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("缺 sid: %v", err)
	}
	if err := s.SubmitSuggestion(ctx0, SuggestionInput{Text: "美甲", Sid: "短"}); codeOf(err) != xerr.ParamError {
		t.Fatalf("sid 格式: %v", err)
	}
	// 疑似手机号：静默丢弃（成功返回，且不查库不写库）
	if err := s.SubmitSuggestion(ctx0, SuggestionInput{Text: "13800138000", Sid: sid}); err != nil {
		t.Fatalf("疑似联系方式应静默丢弃: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("被丢弃的文本不应触达数据库: %v", err)
	}
}

func TestSubmitSuggestionDedupeRateLimitAndInsert(t *testing.T) {
	// 同 sid 同文本同日：已有 → 不再写
	s, mock := sqlmockService(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM `afv_plaza_industry_suggestion`")).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	if err := s.SubmitSuggestion(ctx0, SuggestionInput{Text: "美甲", Sid: sid, IPHash: "iphash"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// 每 sid / IP 每天 ≤3：已有 3 条 → RATE_LIMITED
	s, mock = sqlmockService(t)
	mock.ExpectQuery(`SELECT count\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`SELECT count\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(3))
	err := s.SubmitSuggestion(ctx0, SuggestionInput{Text: "美甲", Sid: sid, IPHash: "iphash"})
	if reasonOf(err) != ReasonSuggestionRateLimited || codeOf(err) != xerr.Conflict {
		t.Fatalf("第 4 条应被限频: %v", err)
	}

	// 正常写入：text 与 norm_text 都落；无文本只落 sid / ip_hash（text / norm_text 为 NULL）
	s, mock = sqlmockService(t)
	mock.ExpectQuery(`SELECT count\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`SELECT count\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))
	mock.ExpectExec("INSERT INTO `afv_plaza_industry_suggestion`").WillReturnResult(sqlmock.NewResult(1, 1))
	if err := s.SubmitSuggestion(ctx0, SuggestionInput{Text: " 美 甲 ", Sid: sid, IPHash: "iphash", UserID: 9}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	s, mock = sqlmockService(t)
	mock.ExpectQuery(`SELECT count\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0)) // 无文本不查「同文本去重」，只查限频
	mock.ExpectExec("INSERT INTO `afv_plaza_industry_suggestion`").WillReturnResult(sqlmock.NewResult(1, 1))
	if err := s.SubmitSuggestion(ctx0, SuggestionInput{Sid: sid, IPHash: "iphash"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
