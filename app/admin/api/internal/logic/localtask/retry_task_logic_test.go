package localtask

import "testing"

func TestParseOriginRef(t *testing.T) {
	cases := []struct {
		ref    string
		prefix string
		id     int64
		ok     bool
	}{
		{"img:123", "img", 123, true},
		{" vid:9 ", "vid", 9, true},
		{"dh:1", "dh", 1, true},
		{"tyo:7", "tyo", 7, true},
		{"pse:8", "pse", 8, true},
		{"simg:9", "simg", 9, true},
		{"note:0", "", 0, false},
		{"img:", "", 0, false},
		{"img:-3", "", 0, false},
		{"img", "", 0, false},
		{":5", "", 0, false},
		{"img:12a", "", 0, false},
	}
	for _, c := range cases {
		prefix, id, ok := parseOriginRef(c.ref)
		if ok != c.ok || prefix != c.prefix || id != c.id {
			t.Errorf("parseOriginRef(%q) = (%q, %d, %v), want (%q, %d, %v)", c.ref, prefix, id, ok, c.prefix, c.id, c.ok)
		}
	}
}
