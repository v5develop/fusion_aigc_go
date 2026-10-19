package svc

import "testing"

// 同步定义里的配置值进 SQL：单引号必须转义，空值落 NULL，避免配置里的引号打破语句。
func TestSQLLiteral(t *testing.T) {
	cases := map[string]string{
		"9:16":  "'9:16'",
		"480p":  "'480p'",
		"":      "NULL",
		"it's":  "'it''s'",
		"a'b'c": "'a''b''c'",
	}
	for in, want := range cases {
		if got := sqlLiteral(in); got != want {
			t.Errorf("sqlLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}
