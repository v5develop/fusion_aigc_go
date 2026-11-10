package common

import "testing"

func TestResolvePresignContentType(t *testing.T) {
	cases := []struct {
		name, snake, camel, want string
	}{
		{"snake only", "image/png", "", "image/png"},
		{"camel only (Java style)", "", "image/png", "image/png"},
		{"both: snake wins", "image/jpeg", "image/png", "image/jpeg"},
		{"blank snake falls back to camel", "  ", "image/webp", "image/webp"},
		{"neither", "", "", ""},
	}
	for _, c := range cases {
		if got := resolvePresignContentType(c.snake, c.camel); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
