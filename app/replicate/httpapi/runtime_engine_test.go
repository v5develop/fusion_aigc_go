package httpapi

import (
	"context"
	"testing"

	"aigc-local-life/app/replicate/core"
)

// 调度器车道并发：库里有且合法的项覆盖 yaml，非法 / 越界忽略；没读过库时用 yaml。
func TestEngineSizesDBOverridesYAML(t *testing.T) {
	p := applyParams(core.DefaultConfig(), true, map[string]string{
		KeyEngineConcurrency: "24", KeyAnalysisConcurrency: "99", KeyDeliveryConcurrency: "x",
	})
	if p.Engine != (core.EngineSizes{Job: 24}) {
		t.Fatalf("只有合法的 job 并发生效：%+v", p.Engine)
	}
	yaml := core.EngineSizes{Job: 16, Analysis: 8, Delivery: 6}
	load := func(context.Context) (map[string]string, error) {
		return map[string]string{KeyDeliveryConcurrency: "3"}, nil
	}
	rt := newRuntime(core.DefaultConfig(), true, load, func(cfg core.Config, _ bool) *core.Service {
		return core.New(cfg, nil, nil)
	})
	rt.engineYAML = yaml
	if got := rt.EngineSizes(); got != yaml {
		t.Fatalf("没读过库时用 yaml：%+v", got)
	}
	rt.Service(context.Background())
	if got := rt.EngineSizes(); got != (core.EngineSizes{Job: 16, Analysis: 8, Delivery: 3}) {
		t.Fatalf("库里的入库并发应覆盖：%+v", got)
	}
}

// 车道并发合计超过「连接池 − 余量」时按比例压低（每车道至少 1）并给出原因；不超或不限时原样。
func TestCapPoolScalesDown(t *testing.T) {
	z := core.EngineSizes{Job: 32, Analysis: 16, Delivery: 16}
	got, why := capPool(z, 40) // 允许 40 − 15 = 25
	if why == "" || got.Job+got.Analysis+got.Delivery > 25 || got.Job < got.Analysis || got.Delivery < 1 {
		t.Fatalf("应按比例压低：%+v %q", got, why)
	}
	if got, why := capPool(z, 0); why != "" || got != z {
		t.Fatalf("池不限时不压：%+v", got)
	}
	if got, why := capPool(core.EngineSizes{Job: 16, Analysis: 8, Delivery: 8}, 64); why != "" || got.Job != 16 {
		t.Fatalf("够用时不压：%+v %q", got, why)
	}
	rt := newRuntime(core.DefaultConfig(), true, nil, nil)
	rt.engineYAML = z
	rt.poolMax = func() int { return 20 } // 允许 5
	if got := rt.EngineSizes(); got.Job+got.Analysis+got.Delivery > 5 {
		t.Fatalf("运行时也要按池封顶：%+v", got)
	}
}
