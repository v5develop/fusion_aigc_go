package httpapi

import (
	"context"
	"errors"
	"testing"

	"aigc-local-life/app/replicate/core"
)

// 总开关：数据库 > 文件配置。库里有合法值以库为准；没配 / 值非法 / 读库失败用 Replicate.Enabled。
func TestEnabledDBOverridesYAML(t *testing.T) {
	cases := []struct {
		name string
		yaml bool
		kv   map[string]string
		err  error
		want bool
	}{
		{"库开文件关", false, map[string]string{KeyEnabled: "true"}, nil, true},
		{"库关文件开", true, map[string]string{KeyEnabled: "false"}, nil, false},
		{"库没配用文件开", true, map[string]string{}, nil, true},
		{"库没配用文件关", false, map[string]string{}, nil, false},
		{"库值非法用文件", true, map[string]string{KeyEnabled: "maybe"}, nil, true},
		{"库空串用文件", true, map[string]string{KeyEnabled: ""}, nil, true},
		{"读库失败用文件", true, nil, errors.New("db down"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := newRuntime(core.DefaultConfig(), false,
				func(context.Context) (map[string]string, error) { return c.kv, c.err },
				func(cfg core.Config, _ bool) *core.Service { return core.New(cfg, nil, nil) })
			rt.enabledYAML = c.yaml
			if _, on := rt.Service(context.Background()); on != c.want {
				t.Fatalf("enabled=%v, want %v", on, c.want)
			}
		})
	}
}
