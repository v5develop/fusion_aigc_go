package logic

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 中文提示词按字节截断不能切出半个字符（MySQL utf8mb4 列会报 Error 1366，素材库转存反复失败）
func TestTruncKeepsUTF8Boundary(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"同一位模特", 3, "同"},
		{"同一位模特", 4, "同"},
		{"同一位模特", 5, "同"},
		{"同一位模特", 6, "同一"},
		{"a同", 2, "a"},
		{"同", 1, ""},
	}
	for _, c := range cases {
		if got := trunc(c.in, c.n); got != c.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	long := strings.Repeat("同一位模特，参考图", 200)
	got := trunc(long, 2000)
	if !utf8.ValidString(got) || len(got) > 2000 || len(got) < 1997 {
		t.Fatalf("trunc long: valid=%v len=%d", utf8.ValidString(got), len(got))
	}
}
