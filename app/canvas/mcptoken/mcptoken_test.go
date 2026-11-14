package mcptoken

import (
	"context"
	"strings"
	"testing"
)

func TestGenerateFormatAndHash(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		plain, hash, display, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if !Looks(plain) || !HasPrefix(plain) || len(plain) != 47 {
			t.Fatalf("形状不对: %q", plain)
		}
		if hash != Hash(plain) || len(hash) != 64 || strings.Contains(hash, plain) {
			t.Fatalf("哈希不对: %s", hash)
		}
		if display != plain[:DisplayPrefixLen] || len(display) != 12 {
			t.Fatalf("前缀不对: %s", display)
		}
		if seen[plain] {
			t.Fatal("重复")
		}
		seen[plain] = true
	}
}

func TestLooks(t *testing.T) {
	for _, bad := range []string{"", "mcp_", "mcp_short", "0123456789abcdef0123456789abcdef", "a.b.c", "mcp_" + strings.Repeat("A", 42) + "!", "MCP_" + strings.Repeat("A", 43)} {
		if Looks(bad) {
			t.Fatalf("%q 不应像 MCP 令牌", bad)
		}
	}
}

func TestScopeAndTTL(t *testing.T) {
	if !ValidScope(ScopeRW) || !ValidScope(ScopeRWGenerate) || ValidScope("canvas") || ValidScope("") {
		t.Fatal("scope")
	}
	if AllowsGenerate(ScopeRW) || !AllowsGenerate(ScopeRWGenerate) {
		t.Fatal("generate")
	}
	for _, d := range []int32{7, 30, 90} {
		if !ValidTTL(d) {
			t.Fatal(d)
		}
	}
	for _, d := range []int32{0, 1, 8, 365, -7} {
		if ValidTTL(d) {
			t.Fatal(d)
		}
	}
	if _, ok := GrantFrom(context.Background()); ok {
		t.Fatal("空 context 不应有 Grant")
	}
	if g, ok := GrantFrom(WithGrant(context.Background(), Grant{UserID: 1, Scope: ScopeRW})); !ok || g.UserID != 1 {
		t.Fatal("grant")
	}
}

func TestAllowsTool(t *testing.T) {
	cases := []struct {
		scope     string
		generates bool
		want      bool
	}{
		{ScopeRW, false, true}, {ScopeRW, true, false},
		{ScopeRWGenerate, false, true}, {ScopeRWGenerate, true, true},
		{"", false, false}, {"bogus", false, false}, {"", true, false},
	}
	for _, c := range cases {
		if got := AllowsTool(c.scope, c.generates); got != c.want {
			t.Fatalf("AllowsTool(%q,%v)=%v", c.scope, c.generates, got)
		}
	}
}
