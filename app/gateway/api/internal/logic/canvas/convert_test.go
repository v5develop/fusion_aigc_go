package canvas

import (
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/pkg/xerr"
)

func TestSnapshotText(t *testing.T) {
	s, err := snapshotText(json.RawMessage(`{"nodes":[],"n":12345678901234567890}`))
	if err != nil || s != `{"nodes":[],"n":12345678901234567890}` {
		t.Fatalf("原样透传: %s %v", s, err)
	}
	if s, err := snapshotText(json.RawMessage(`"{\"nodes\":[]}"`)); err != nil || s != `{"nodes":[]}` {
		t.Fatalf("字符串形式的快照解一层引号: %s %v", s, err)
	}
	big := json.RawMessage(`{"x":"` + strings.Repeat("a", MaxSnapshotBytes) + `"}`)
	if _, err := snapshotText(big); err == nil {
		t.Fatal("超 2MB 应在网关拦下")
	} else if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("1003: %v", err)
	}
	if _, err := snapshotText(nil); err == nil {
		t.Fatal("缺 snapshot")
	}
}
